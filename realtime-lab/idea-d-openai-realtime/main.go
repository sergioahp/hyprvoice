// idea-d: OpenAI Realtime streaming draft + gpt-4o-transcribe final
//
// DRAFT path: persistent WebSocket to gpt-4o-realtime-preview (transcription mode)
// receives incremental delta events -> emits as draft, promotes completed turns to committed.
//
// FINAL path: at stop, one batch POST /v1/audio/transcriptions with gpt-4o-transcribe
// over the full buffered PCM -> emits as authoritative final.
//
// stdout: JSONL events per INTERFACE.md
// stderr: all logs, control socket path
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---- stdout JSONL emission ----

type outEvent struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
}

var outMu sync.Mutex

func emit(e outEvent) {
	outMu.Lock()
	defer outMu.Unlock()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
}

func emitStatus(s string)    { emit(outEvent{Type: "status", State: s}) }
func emitDraft(t string)     { emit(outEvent{Type: "draft", Text: t}) }
func emitCommitted(t string) { emit(outEvent{Type: "committed", Text: t}) }
func emitFinal(t string)     { emit(outEvent{Type: "final", Text: t}) }
func emitInfo(t string)      { emit(outEvent{Type: "info", Text: t}) }
func emitError(t string)     { emit(outEvent{Type: "error", Text: t}) }
func emitReset()             { emit(outEvent{Type: "reset"}) }

// ---- application state ----

var apiKey string

type appState struct {
	mu sync.Mutex

	status string // "idle" / "recording" / "transcribing"

	// WS write lock: gorilla requires serial writes
	wsMu   sync.Mutex
	wsConn *websocket.Conn

	wsCancel      context.CancelFunc
	captureCancel context.CancelFunc

	// transcript (non-overlap rule: committed is prefix, draftBuf is tail)
	committed string
	draftBuf  string

	// full PCM at 16kHz s16le for batch call
	pcmBuf []byte

	// turn tracking for finalization
	turnActive bool
	turnDone   chan struct{}

	// log the first N raw server events for format debugging
	rawEventCount int
}

var app = &appState{
	status:   "idle",
	turnDone: make(chan struct{}, 2),
}

// ---- WAV helpers ----

// wavPCM extracts the raw PCM payload from a WAV file by scanning for the data chunk.
func wavPCM(data []byte) ([]byte, error) {
	for i := 0; i+8 < len(data); i++ {
		if string(data[i:i+4]) == "data" {
			size := int(binary.LittleEndian.Uint32(data[i+4:]))
			start := i + 8
			end := start + size
			if end > len(data) {
				end = len(data)
			}
			return data[start:end], nil
		}
	}
	return nil, fmt.Errorf("no data chunk found in WAV file")
}

// wavWrap packs 16kHz mono s16le PCM into a minimal WAV container for the batch API.
func wavWrap(pcm []byte) []byte {
	const (
		rate      = 16000
		channels  = 1
		bitsPerSa = 16
	)
	byteRate := rate * channels * bitsPerSa / 8
	blockAlign := channels * bitsPerSa / 8
	out := make([]byte, 44+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], channels)
	binary.LittleEndian.PutUint32(out[24:], rate)
	binary.LittleEndian.PutUint32(out[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:], bitsPerSa)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

// ---- resampling: 16kHz -> 24kHz ----
// OpenAI Realtime (gpt-4o-realtime-preview) expects 24kHz PCM16.

func resample16to24(in []byte) []byte {
	n := len(in) / 2 // number of s16 samples in
	m := n * 3 / 2   // number of s16 samples out (ratio 24/16 = 1.5)
	out := make([]byte, m*2)
	for i := 0; i < m; i++ {
		pos := float64(i) * 16.0 / 24.0
		idx := int(pos)
		frac := pos - float64(idx)
		var s0, s1 int16
		if idx*2+1 < len(in) {
			s0 = int16(in[idx*2]) | int16(in[idx*2+1])<<8
		}
		if (idx+1)*2+1 < len(in) {
			s1 = int16(in[(idx+1)*2]) | int16(in[(idx+1)*2+1])<<8
		} else {
			s1 = s0
		}
		s := int16(float64(s0)*(1-frac) + float64(s1)*frac)
		out[i*2] = byte(s)
		out[i*2+1] = byte(s >> 8)
	}
	return out
}

// ---- OpenAI Realtime WS protocol structs (GA transcription API) ----

// wsTranscriptionSessionUpdate is the correct message for the GA ?intent=transcription
// endpoint. It uses "transcription_session.update" (not "session.update") and flat
// fields directly in "session" (not nested under audio.input).
type wsTranscriptionSessionUpdate struct {
	Type    string                      `json:"type"`
	Session wsTranscriptionSessionConfig `json:"session"`
}

type wsTranscriptionSessionConfig struct {
	InputAudioFormat      string          `json:"input_audio_format"`
	InputAudioTranscription wsTranscription `json:"input_audio_transcription"`
	TurnDetection         *wsTurnDetect   `json:"turn_detection,omitempty"`
	InputAudioNoiseReduce *wsNoiseReduce  `json:"input_audio_noise_reduction,omitempty"`
}

// pcm16: 16-bit PCM at 24kHz mono little-endian (GA spec)
type wsNoiseReduce struct {
	Type string `json:"type"` // "near_field"
}
type wsTranscription struct {
	Model    string `json:"model"`
	Language string `json:"language,omitempty"`
}
type wsTurnDetect struct {
	Type              string  `json:"type"`
	Threshold         float64 `json:"threshold"`
	PrefixPaddingMs   int     `json:"prefix_padding_ms"`
	SilenceDurationMs int     `json:"silence_duration_ms"`
}
type wsAudioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio"` // base64-encoded PCM16 at 24kHz
}
type wsAudioCommit struct {
	Type string `json:"type"`
}

// Server events (only fields we care about)
type wsServerEvent struct {
	Type       string  `json:"type"`
	ItemID     string  `json:"item_id,omitempty"`
	Delta      string  `json:"delta,omitempty"`
	Transcript string  `json:"transcript,omitempty"`
	Error      *wsErr  `json:"error,omitempty"`
	Session    *wsSess `json:"session,omitempty"`
}
type wsSess struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}
type wsErr struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---- WebSocket session ----

func wsConnect(ctx context.Context) error {
	// GA transcription endpoint: ?intent=transcription, no OpenAI-Beta header
	url := "wss://api.openai.com/v1/realtime?intent=transcription"
	hdr := http.Header{
		"Authorization": {"Bearer " + apiKey},
	}
	log.Printf("ws: connecting to %s", url)
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, url, hdr)
	if err != nil {
		if resp != nil {
			log.Printf("ws: dial failed status=%d", resp.StatusCode)
		}
		return fmt.Errorf("ws dial: %w", err)
	}

	app.mu.Lock()
	app.wsConn = conn
	app.mu.Unlock()

	// GA transcription session config: "transcription_session.update" with flat
	// input_audio_format="pcm16" (not nested audio.input structure).
	// pcm16 = 16-bit PCM, 24kHz, mono, little-endian.
	// GA endpoint requires message type "session.update" (NOT
	// "transcription_session.update", which the server rejects), with the nested
	// session.type="transcription" + audio.input{...} schema echoed back in
	// session.created. Built as a map to mirror that schema exactly.
	cfg := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				// Per OpenAI GA docs: for transcription-only sessions, omit
				// turn_detection (null) and commit audio manually. server_vad was
				// mis-segmenting into tiny turns -> hallucinated fragments.
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": 24000},
					"transcription":  map[string]any{"model": "gpt-4o-transcribe", "language": "en"},
					"turn_detection": nil,
				},
			},
		},
	}
	app.wsMu.Lock()
	err = conn.WriteJSON(cfg)
	app.wsMu.Unlock()
	if err != nil {
		conn.Close()
		app.mu.Lock()
		app.wsConn = nil
		app.mu.Unlock()
		return fmt.Errorf("ws: write session config: %w", err)
	}
	log.Printf("ws: session configured (session.update, audio/pcm@24k, gpt-4o-transcribe)")
	return nil
}

// wireBuf captures the exact 24kHz PCM bytes sent over the WS, for debugging
// whether the server receives correct audio.
var (
	wireBuf []byte
	wireMu  sync.Mutex
)

func wsReadLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		app.mu.Lock()
		conn := app.wsConn
		app.mu.Unlock()
		if conn == nil {
			return
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-ctx.Done():
				// normal shutdown
			default:
				log.Printf("ws: read error: %v", err)
			}
			return
		}

		// log first 8 raw server events to verify session config was applied
		app.mu.Lock()
		n := app.rawEventCount
		app.rawEventCount++
		app.mu.Unlock()
		if n < 8 {
			log.Printf("ws: raw[%d]: %s", n, string(raw[:min(len(raw), 400)]))
		}

		var ev wsServerEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Printf("ws: parse error: %v (raw=%s)", err, string(raw[:min(len(raw), 200)]))
			continue
		}
		handleWsEvent(ev)
	}
}

func handleWsEvent(ev wsServerEvent) {
	switch ev.Type {
	case "session.created":
		if ev.Session != nil {
			log.Printf("ws: session created id=%s model=%s", ev.Session.ID, ev.Session.Model)
		}
	case "session.updated":
		log.Printf("ws: session updated")

	case "error":
		if ev.Error != nil {
			log.Printf("ws: server error code=%s msg=%s", ev.Error.Code, ev.Error.Message)
			// committed empty buffer errors are expected on stop - don't alarm the user
			if ev.Error.Code != "invalid_request_error" {
				emitError("realtime: " + ev.Error.Message)
			}
		}

	case "input_audio_buffer.speech_started":
		log.Printf("ws: speech started")
		app.mu.Lock()
		app.turnActive = true
		app.mu.Unlock()

	case "input_audio_buffer.speech_stopped":
		log.Printf("ws: speech stopped item=%s", ev.ItemID)

	case "input_audio_buffer.committed":
		log.Printf("ws: audio committed item=%s", ev.ItemID)

	case "conversation.item.input_audio_transcription.delta":
		if ev.Delta != "" {
			app.mu.Lock()
			app.draftBuf += ev.Delta
			d := app.draftBuf
			app.mu.Unlock()
			emitDraft(d)
		}

	case "conversation.item.input_audio_transcription.completed":
		// A turn has been fully transcribed. Promote to committed, clear draft.
		log.Printf("ws: transcription completed: %q", ev.Transcript)
		app.mu.Lock()
		t := ev.Transcript
		if t != "" {
			if app.committed == "" {
				app.committed = t
			} else {
				app.committed += " " + t
			}
		}
		c := app.committed
		app.draftBuf = ""
		app.turnActive = false
		app.mu.Unlock()

		if t != "" {
			emitCommitted(c)
		}
		emitDraft("")

		select {
		case app.turnDone <- struct{}{}:
		default:
		}

	case "conversation.item.input_audio_transcription.failed":
		log.Printf("ws: transcription failed item=%s", ev.ItemID)
		if ev.Error != nil {
			emitError("transcription failed: " + ev.Error.Message)
		}
		app.mu.Lock()
		app.turnActive = false
		app.mu.Unlock()
		select {
		case app.turnDone <- struct{}{}:
		default:
		}

	case "rate_limits.updated":
		// ignore

	default:
		log.Printf("ws: unhandled event: %s", ev.Type)
	}
}

func wsSendChunk(pcm []byte) error {
	app.mu.Lock()
	conn := app.wsConn
	app.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("ws: not connected")
	}

	r24 := resample16to24(pcm)
	enc := base64.StdEncoding.EncodeToString(r24)

	wireMu.Lock()
	wireBuf = append(wireBuf, r24...)
	wireMu.Unlock()

	app.wsMu.Lock()
	err := conn.WriteJSON(wsAudioAppend{Type: "input_audio_buffer.append", Audio: enc})
	app.wsMu.Unlock()
	return err
}

// wsCommitAndWait sends input_audio_buffer.commit and waits for transcription.completed.
// This ensures any speech in flight gets finalized before we close the session.
func wsCommitAndWait(ctx context.Context) {
	app.mu.Lock()
	conn := app.wsConn
	app.mu.Unlock()
	if conn == nil {
		return
	}

	// drain any stale signal from a previous turn
	select {
	case <-app.turnDone:
	default:
	}

	app.wsMu.Lock()
	err := conn.WriteJSON(wsAudioCommit{Type: "input_audio_buffer.commit"})
	app.wsMu.Unlock()
	if err != nil {
		log.Printf("ws: commit error: %v", err)
		return
	}
	log.Printf("ws: commit sent, waiting for transcription.completed")

	// wait for the in-flight turn to complete (5s max)
	select {
	case <-app.turnDone:
		log.Printf("ws: turn finalized")
	case <-time.After(5 * time.Second):
		log.Printf("ws: finalize timeout (no speech in buffer?)")
	case <-ctx.Done():
	}
}

func wsClose() {
	wireMu.Lock()
	if len(wireBuf) > 0 {
		if e := os.WriteFile("wire_sent_24k.raw", wireBuf, 0644); e == nil {
			log.Printf("wire: dumped %d bytes (24kHz s16le) to wire_sent_24k.raw", len(wireBuf))
		}
	}
	wireMu.Unlock()

	app.mu.Lock()
	conn := app.wsConn
	cancel := app.wsCancel
	app.wsConn = nil
	app.wsCancel = nil
	app.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		app.wsMu.Lock()
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		app.wsMu.Unlock()
		conn.Close()
	}
}

// ---- batch transcription ----

func batchTranscribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", fmt.Errorf("no audio captured")
	}
	wav := wavWrap(pcm)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if err := mw.WriteField("model", "gpt-4o-transcribe"); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}

	// use explicit Content-Type so OpenAI doesn't have to guess
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	h.Set("Content-Type", "audio/wav")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := pw.Write(wav); err != nil {
		return "", fmt.Errorf("write wav: %w", err)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, string(rb))
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	log.Printf("batch: completed in %v: %q", time.Since(t0), out.Text)
	return out.Text, nil
}

// ---- audio pump ----

// pumpAudio reads chunks from r, resamples and sends to WS, and accumulates PCM buffer.
// If pace > 0, sleeps between chunks to simulate real-time playback speed.
func pumpAudio(ctx context.Context, r io.Reader, chunkSz int, pace time.Duration) {
	buf := make([]byte, chunkSz)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := io.ReadFull(r, buf)
		if n > 0 {
			ch := make([]byte, n)
			copy(ch, buf[:n])

			app.mu.Lock()
			app.pcmBuf = append(app.pcmBuf, ch...)
			app.mu.Unlock()

			if e := wsSendChunk(ch); e != nil {
				log.Printf("pump: send error: %v", e)
			}
		}
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Printf("pump: read error: %v", err)
			}
			return
		}
		if pace > 0 {
			select {
			case <-time.After(pace):
			case <-ctx.Done():
				return
			}
		}
	}
}

// ---- session lifecycle ----

func startSession(audioFn func(context.Context)) {
	// stop any running session first
	app.mu.Lock()
	oldCapCancel := app.captureCancel
	app.mu.Unlock()
	if oldCapCancel != nil {
		oldCapCancel()
	}
	wsClose()
	time.Sleep(60 * time.Millisecond) // let goroutines exit

	// reset state
	app.mu.Lock()
	app.committed = ""
	app.draftBuf = ""
	app.pcmBuf = nil
	app.turnActive = false
	app.status = "recording"
	app.rawEventCount = 0
	app.mu.Unlock()

	// drain stale turn signal
	select {
	case <-app.turnDone:
	default:
	}

	emitReset()
	emitStatus("recording")

	// open WebSocket
	wsCtx, wsCancel := context.WithCancel(context.Background())
	app.mu.Lock()
	app.wsCancel = wsCancel
	app.mu.Unlock()

	if err := wsConnect(wsCtx); err != nil {
		emitError("ws connect: " + err.Error())
		wsCancel()
		app.mu.Lock()
		app.status = "idle"
		app.mu.Unlock()
		emitStatus("idle")
		return
	}
	go wsReadLoop(wsCtx)

	// start audio source
	captureCtx, captureCancel := context.WithCancel(context.Background())
	app.mu.Lock()
	app.captureCancel = captureCancel
	app.mu.Unlock()

	go func() {
		audioFn(captureCtx)
		log.Printf("audio source finished")
	}()

	// NOTE: no periodic/VAD commit. OpenAI's realtime transcription is accurate
	// ONLY on a complete utterance: committing incremental segments (server_vad OR
	// fixed ~2.5s manual commits) makes the model hallucinate on mid-phrase cuts
	// (e.g. "it's about a meter away" -> "giveaway"). So we commit ONCE at stop and
	// the full transcript streams word-by-word as deltas. This means the draft is
	// not live during speech - a fundamental limitation of this API for dictation.
	// For a true live draft use Voxtral realtime (idea A / idea E).
}

func doReplay(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		emitError("read wav: " + err.Error())
		return
	}
	pcm, err := wavPCM(raw)
	if err != nil {
		emitError("parse wav: " + err.Error())
		return
	}
	dur := float64(len(pcm)) / 32000.0 // 16kHz * 2 bytes
	log.Printf("replay: %.1fs (%.0f bytes) from %s", dur, float64(len(pcm)), path)
	emitInfo(fmt.Sprintf("replay %.1fs from %s", dur, path))

	startSession(func(ctx context.Context) {
		// 20ms chunks paced at realtime: 640 bytes per chunk
		pumpAudio(ctx, bytes.NewReader(pcm), 640, 20*time.Millisecond)
	})
}

func doMic() {
	startSession(func(ctx context.Context) {
		cmd := exec.CommandContext(ctx, "pw-record", "--format", "s16", "--rate", "16000", "--channels", "1", "-")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			emitError("pw-record pipe: " + err.Error())
			return
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			emitError("pw-record start: " + err.Error())
			return
		}
		// 100ms chunks, no artificial pacing (mic delivers at real time)
		pumpAudio(ctx, stdout, 3200, 0)
		_ = cmd.Wait()
	})
}

func doStop() {
	app.mu.Lock()
	status := app.status
	capCancel := app.captureCancel
	app.mu.Unlock()

	if status == "idle" {
		log.Printf("stop: already idle")
		return
	}

	// stop audio source
	if capCancel != nil {
		capCancel()
	}
	time.Sleep(100 * time.Millisecond) // let pump drain

	// commit any pending audio and wait for realtime to finish transcribing
	wsCommitAndWait(context.Background())

	// capture PCM buffer before closing WS
	app.mu.Lock()
	pcm := make([]byte, len(app.pcmBuf))
	copy(pcm, app.pcmBuf)
	app.mu.Unlock()

	wsClose()

	// run authoritative batch transcription
	app.mu.Lock()
	app.status = "transcribing"
	app.mu.Unlock()
	emitStatus("transcribing")
	dur := float64(len(pcm)) / 32000.0
	emitInfo(fmt.Sprintf("batch: gpt-4o-transcribe on %.1fs of audio...", dur))

	bCtx, bCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer bCancel()

	final, err := batchTranscribe(bCtx, pcm)
	if err != nil {
		log.Printf("batch error: %v", err)
		emitError("batch: " + err.Error())
	} else {
		emitFinal(final)
	}

	app.mu.Lock()
	app.status = "idle"
	app.mu.Unlock()
	emitStatus("idle")
}

// ---- control socket ----

func handleControl(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := parts[0]
		args := parts[1:]

		switch cmd {
		case "replay":
			path := "../sample.wav"
			if len(args) > 0 {
				path = args[0]
			}
			fmt.Fprintln(conn, "ok: replay "+path)
			go doReplay(path)

		case "mic":
			fmt.Fprintln(conn, "ok: mic")
			go doMic()

		case "stop":
			fmt.Fprintln(conn, "ok: stop")
			go doStop()

		case "status":
			app.mu.Lock()
			s := app.status
			app.mu.Unlock()
			fmt.Fprintln(conn, s)

		case "quit":
			fmt.Fprintln(conn, "ok: quit")
			go func() {
				doStop()
				os.Exit(0)
			}()

		default:
			fmt.Fprintln(conn, "error: unknown command: "+cmd)
		}
	}
}

// ---- main ----

func main() {
	log.SetFlags(log.Ltime)
	log.SetOutput(os.Stderr)

	apiKey = os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	sockPath, _ := filepath.Abs("control.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("listen unix %s: %v", sockPath, err)
	}
	defer ln.Close()

	fmt.Fprintf(os.Stderr, "control socket: %s\n", sockPath)
	emitStatus("idle")

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go handleControl(conn)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
