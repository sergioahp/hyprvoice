// idea-a: Mistral Voxtral realtime draft + gpt-4o-transcribe strong final
//
// DRAFT path: persistent WebSocket to Mistral Voxtral realtime
// (wss://api.mistral.ai/v1/audio/transcriptions/realtime) streams PCM16 at
// 16 kHz directly (no resampling). Receives transcription.delta events ->
// emits as draft; transcription.done (segment finalized) -> promotes to committed.
//
// FINAL path: at stop, one batch POST to /v1/audio/transcriptions (OpenAI
// gpt-4o-transcribe) over the full buffered PCM -> emits as authoritative final.
//
// PROTOCOL NOTE: Mistral's hosted realtime API uses a WebSocket protocol
// documented at docs.mistral.ai. The wire format is OpenAI-realtime-compatible
// (session.update / input_audio_buffer.append / input_audio_buffer.commit) but
// without the nested audio.input session config. Model is passed both as a URL
// query param and in the session.update message. target_streaming_delay_ms
// controls latency (80..2400ms; default 480ms).
//
// stdout: JSONL events per INTERFACE.md (nothing else)
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

// emitText writes a JSONL line that always includes "text" (even empty string),
// required for draft/committed/final so the visualizer can clear the field.
func emitText(typ, text string) {
	outMu.Lock()
	defer outMu.Unlock()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: typ, Text: text})
}

func emitStatus(s string)    { emit(outEvent{Type: "status", State: s}) }
func emitDraft(t string)     { emitText("draft", t) }
func emitCommitted(t string) { emitText("committed", t) }
func emitFinal(t string)     { emitText("final", t) }
func emitInfo(t string)      { emit(outEvent{Type: "info", Text: t}) }
func emitError(t string)     { emit(outEvent{Type: "error", Text: t}) }
func emitReset()             { emit(outEvent{Type: "reset"}) }

// ---- application state ----

var (
	mistralKey string
	openAIKey  string
)

type appState struct {
	mu sync.Mutex

	status string // "idle" / "recording" / "transcribing"

	// WS write lock: gorilla requires serial writes from one goroutine at a time
	wsMu   sync.Mutex
	wsConn *websocket.Conn

	wsCancel      context.CancelFunc
	captureCancel context.CancelFunc

	// transcript state (non-overlap: committed is prefix, draftBuf is tail)
	committed string
	draftBuf  string

	// full 16kHz s16le PCM for batch call at stop
	pcmBuf []byte

	// signaled when we receive transcription.done after a commit
	segDone chan struct{}
}

var app = &appState{
	status:  "idle",
	segDone: make(chan struct{}, 2),
}

// ---- WAV helpers ----

// wavPCM extracts raw PCM from a WAV file by scanning for the data chunk.
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
	return nil, fmt.Errorf("no data chunk in WAV file")
}

// wavWrap packs 16kHz mono s16le PCM into a WAV container for the batch API.
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
	binary.LittleEndian.PutUint16(out[20:], 1)
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

// ---- Voxtral WebSocket protocol messages ----

// Voxtral accepts 16kHz PCM16 directly (no resampling needed unlike OpenAI realtime).
// Mistral hosted API protocol (confirmed from error responses and SDK):
//   session.update -> nested "session" object with audio_format + target_streaming_delay_ms
//                     model is passed only as a URL query param, not in the message
//   input_audio_buffer.append -> stream base64-encoded PCM16 chunks
//   input_audio_buffer.flush -> periodic flush (get current partial result)
//   input_audio_buffer.end   -> signal end of utterance (no more audio)
//
// Server sends: session.created, session.updated,
//               transcription.text.delta (incremental), transcription.done,
//               transcription.segment, transcription.language, error

type vsAudioFormat struct {
	Encoding   string `json:"encoding"`    // "pcm_s16le"
	SampleRate int    `json:"sample_rate"` // 16000
}

type vsSessionConfig struct {
	AudioFormat   vsAudioFormat `json:"audio_format"`
	TargetDelayMs int           `json:"target_streaming_delay_ms,omitempty"`
}

type vsSessionUpdate struct {
	Type    string          `json:"type"`    // "session.update"
	Session vsSessionConfig `json:"session"` // required nested field
}

type vsAudioAppend struct {
	Type  string `json:"type"`  // "input_audio.append"
	Audio string `json:"audio"` // base64-encoded PCM16 at 16kHz
}

type vsAudioFlush struct {
	Type string `json:"type"` // "input_audio.flush"
}

type vsAudioEnd struct {
	Type string `json:"type"` // "input_audio.end"
}

// vsServerEvent covers all inbound message shapes we care about.
type vsServerEvent struct {
	Type  string      `json:"type"`
	ID    string      `json:"id,omitempty"`   // session.created
	Text  string      `json:"text,omitempty"` // transcription.text.delta / transcription.done
	Error interface{} `json:"error,omitempty"` // error (string or object)
}

// errorString extracts a human-readable string from the Voxtral error field,
// which may be a string or an object with a "message" sub-field.
func errorString(raw interface{}) string {
	if raw == nil {
		return "unknown error"
	}
	switch v := raw.(type) {
	case string:
		return v
	case map[string]interface{}:
		if msg, ok := v["message"].(string); ok {
			return msg
		}
	}
	return fmt.Sprintf("%v", raw)
}

// ---- Voxtral WebSocket session ----

const (
	voxtralModel    = "voxtral-mini-transcribe-realtime-2602"
	voxtralWS       = "wss://api.mistral.ai/v1/audio/transcriptions/realtime"
	streamDelayMs   = 480 // configurable 80..2400; 480ms balances latency and accuracy
)

func vsConnect(ctx context.Context) error {
	url := voxtralWS + "?model=" + voxtralModel
	hdr := http.Header{
		"Authorization": {"Bearer " + mistralKey},
	}
	log.Printf("voxtral: connecting to %s", url)
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, url, hdr)
	if err != nil {
		if resp != nil {
			log.Printf("voxtral: dial failed status=%d", resp.StatusCode)
		}
		return fmt.Errorf("voxtral dial: %w", err)
	}

	app.mu.Lock()
	app.wsConn = conn
	app.mu.Unlock()

	// session.update: configure audio format and target streaming delay.
	// Model is already specified as a URL query param; it must NOT appear in this message.
	cfg := vsSessionUpdate{
		Type: "session.update",
		Session: vsSessionConfig{
			AudioFormat:   vsAudioFormat{Encoding: "pcm_s16le", SampleRate: 16000},
			TargetDelayMs: streamDelayMs,
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
		return fmt.Errorf("voxtral: write session.update: %w", err)
	}
	log.Printf("voxtral: session configured (model=%s, delay=%dms)", voxtralModel, streamDelayMs)
	return nil
}

func vsReadLoop(ctx context.Context) {
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
				log.Printf("voxtral: read error: %v", err)
			}
			return
		}

		var ev vsServerEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			// Unparseable frame = protocol drift. Fail loud, don't swallow.
			emitError("voxtral: unparseable frame: " + err.Error())
			log.Printf("voxtral UNPARSEABLE frame: %v raw=%s", err, string(raw))
			continue
		}
		handleVsEvent(ev, raw)
	}
}

func handleVsEvent(ev vsServerEvent, raw []byte) {
	switch ev.Type {
	case "session.created":
		log.Printf("voxtral: session created id=%s", ev.ID)

	case "session.updated":
		log.Printf("voxtral: session updated")

	case "transcription.text.delta":
		// Incremental draft text from the realtime model.
		if ev.Text != "" {
			app.mu.Lock()
			app.draftBuf += ev.Text
			d := app.draftBuf
			app.mu.Unlock()
			emitDraft(d)
		}

	case "transcription.segment":
		// A sentence/segment has been finalized; promote to committed.
		log.Printf("voxtral: segment: %q", ev.Text)
		app.mu.Lock()
		if ev.Text != "" {
			if app.committed != "" {
				app.committed += " "
			}
			app.committed += ev.Text
		}
		app.draftBuf = ""
		c := app.committed
		app.mu.Unlock()

		if ev.Text != "" {
			emitCommitted(c)
		}
		emitDraft("")

	case "transcription.done":
		// Stream fully complete (end of utterance).
		log.Printf("voxtral: done: %q", ev.Text)
		app.mu.Lock()
		if ev.Text != "" {
			app.committed = ev.Text // cumulative final text from the model
		}
		app.draftBuf = ""
		c := app.committed
		app.mu.Unlock()

		if ev.Text != "" {
			emitCommitted(c)
		}
		emitDraft("")

		select {
		case app.segDone <- struct{}{}:
		default:
		}

	case "transcription.language":
		log.Printf("voxtral: detected language: %s", ev.Text)

	case "error":
		msg := errorString(ev.Error)
		log.Printf("voxtral: error: %s", msg)
		emitError("voxtral: " + msg)

	default:
		// Unknown event type = our mental model is wrong. Surface it loudly
		// with the full frame instead of silently ignoring it.
		emitError("voxtral: UNKNOWN event type " + ev.Type)
		log.Printf("voxtral UNKNOWN EVENT (full frame): %s", string(raw))
	}
}

func vsSendChunk(pcm []byte) error {
	app.mu.Lock()
	conn := app.wsConn
	app.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("voxtral: not connected")
	}

	// Voxtral accepts 16kHz PCM16 directly - no resampling needed
	enc := base64.StdEncoding.EncodeToString(pcm)

	app.wsMu.Lock()
	err := conn.WriteJSON(vsAudioAppend{Type: "input_audio.append", Audio: enc})
	app.wsMu.Unlock()
	return err
}

// vsEndAndWait sends input_audio_buffer.end (signals no more audio) and waits for
// transcription.done to ensure the final segment is captured before we move on.
func vsEndAndWait(ctx context.Context) {
	app.mu.Lock()
	conn := app.wsConn
	app.mu.Unlock()
	if conn == nil {
		return
	}

	// drain any stale signal
	select {
	case <-app.segDone:
	default:
	}

	app.wsMu.Lock()
	err := conn.WriteJSON(vsAudioEnd{Type: "input_audio.end"})
	app.wsMu.Unlock()
	if err != nil {
		log.Printf("voxtral: end error: %v", err)
		return
	}
	log.Printf("voxtral: input_audio.end sent, waiting for transcription.done...")

	select {
	case <-app.segDone:
		log.Printf("voxtral: transcription.done received")
	case <-time.After(8 * time.Second):
		log.Printf("voxtral: timeout waiting for transcription.done")
	case <-ctx.Done():
	}
}

func vsClose() {
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

// ---- batch transcription (OpenAI gpt-4o-transcribe) ----

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
	req.Header.Set("Authorization", "Bearer "+openAIKey)
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
	log.Printf("batch: completed in %v: %q", time.Since(t0).Round(time.Millisecond), out.Text)
	return out.Text, nil
}

// ---- audio pump ----

// pumpAudio reads PCM chunks from r, sends to Voxtral WS, and accumulates the buffer.
// If pace > 0, sleeps between chunks to simulate 1x real-time playback (for WAV replay).
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

			if e := vsSendChunk(ch); e != nil {
				// WS closed (normal at stop) or network error — stop pumping
				log.Printf("pump: send error, stopping: %v", e)
				return
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
	// tear down any previous session
	app.mu.Lock()
	oldCapCancel := app.captureCancel
	app.mu.Unlock()
	if oldCapCancel != nil {
		oldCapCancel()
	}
	vsClose()
	time.Sleep(60 * time.Millisecond) // let goroutines unwind

	// reset state
	app.mu.Lock()
	app.committed = ""
	app.draftBuf = ""
	app.pcmBuf = nil
	app.status = "recording"
	app.mu.Unlock()

	// drain stale segment signal
	select {
	case <-app.segDone:
	default:
	}

	emitReset()
	emitStatus("recording")

	// open Voxtral WebSocket
	wsCtx, wsCancel := context.WithCancel(context.Background())
	app.mu.Lock()
	app.wsCancel = wsCancel
	app.mu.Unlock()

	if err := vsConnect(wsCtx); err != nil {
		log.Printf("voxtral: connect failed: %v", err)
		emitInfo("voxtral unavailable - draft disabled, final only")
		wsCancel()
		// continue recording; we still buffer PCM for the OpenAI batch call
	} else {
		go vsReadLoop(wsCtx)
	}

	// start audio source goroutine
	captureCtx, captureCancel := context.WithCancel(context.Background())
	app.mu.Lock()
	app.captureCancel = captureCancel
	app.mu.Unlock()

	go func() {
		audioFn(captureCtx)
		log.Printf("audio source finished")
	}()
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
	dur := float64(len(pcm)) / 32000.0 // 16kHz * 2 bytes/sample
	log.Printf("replay: %.1fs (%d bytes) from %s", dur, len(pcm), path)
	emitInfo(fmt.Sprintf("replay %.1fs from %s", dur, path))

	startSession(func(ctx context.Context) {
		// 20ms chunks paced at real time: 640 bytes per chunk (= 320 samples)
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
	if status == "transcribing" {
		log.Printf("stop: already transcribing")
		return
	}

	// stop audio source
	if capCancel != nil {
		capCancel()
	}
	time.Sleep(100 * time.Millisecond) // let pump finish its current chunk

	// signal end of audio to Voxtral and wait for final transcription
	vsEndAndWait(context.Background())

	// snapshot PCM before closing WS
	app.mu.Lock()
	pcm := make([]byte, len(app.pcmBuf))
	copy(pcm, app.pcmBuf)
	app.mu.Unlock()

	vsClose()

	// authoritative final via OpenAI gpt-4o-transcribe
	app.mu.Lock()
	app.status = "transcribing"
	app.mu.Unlock()
	emitStatus("transcribing")

	dur := float64(len(pcm)) / 32000.0
	emitInfo(fmt.Sprintf("batch: gpt-4o-transcribe on %.1fs of audio...", dur))
	log.Printf("batch: transcribing %.1fs (%d bytes) with gpt-4o-transcribe", dur, len(pcm))

	bCtx, bCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer bCancel()

	final, err := batchTranscribe(bCtx, pcm)
	if err != nil {
		log.Printf("batch error: %v", err)
		emitError("batch: " + err.Error())
		// fall back to last Voxtral committed text
		app.mu.Lock()
		fb := app.committed
		if app.draftBuf != "" {
			fb += " " + app.draftBuf
		}
		app.mu.Unlock()
		if fb != "" {
			emitFinal(fb)
		}
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

	mistralKey = os.Getenv("MISTRAL_API_KEY")
	openAIKey = os.Getenv("OPENAI_API_KEY")

	if mistralKey == "" {
		log.Println("warning: MISTRAL_API_KEY not set - Voxtral draft disabled, final only")
	}
	if openAIKey == "" {
		fmt.Fprintln(os.Stderr, "fatal: OPENAI_API_KEY not set")
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
