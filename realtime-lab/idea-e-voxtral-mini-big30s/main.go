// idea-e: three-tier transcription cascade
//
// Tier 1 (instant draft):  Mistral Voxtral realtime WebSocket — sub-second
//   partials appear as the speaker talks.
// Tier 2 (refinement):     gpt-4o-mini-transcribe every ~4 s on the uncommitted
//   audio window — replaces the rough voxtral draft with a cleaner version.
// Tier 3 (30 s commit):    gpt-4o-transcribe once per 30 s — authoritative text
//   locked into `committed`; draft resets to only the tail after that point.
//   A final tier-3 call is made on `stop` for the remaining tail.
//
// stdout: JSONL events (status/draft/committed/final/info/error/reset)
// stderr: all logs, control socket path, timing info
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

	"github.com/coder/websocket"
)

// -------- constants --------

const (
	sampleRate     = 16000
	bytesPerSample = 2 // s16le mono
	bytesPerSec    = sampleRate * bytesPerSample

	refineInterval = 4 * time.Second
	commitInterval = 30 * time.Second

	// Minimum audio before triggering a transcription call (0.5 s).
	minAudioBytes = bytesPerSec / 2

	draftModel  = "gpt-4o-mini-transcribe"
	commitModel = "gpt-4o-transcribe"

	voxtralModel    = "voxtral-mini-transcribe-realtime-2602"
	voxtralWSURL    = "wss://api.mistral.ai/v1/audio/transcriptions/realtime"
	voxtralChunkMax = 262144 // max decoded bytes per input_audio.append

	// Replay pacing: 100 ms chunks.
	replayChunk = bytesPerSec / 10
)

// -------- stdout JSONL event stream --------

type event struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
}

var outMu sync.Mutex

func emit(e event) {
	b, _ := json.Marshal(e)
	outMu.Lock()
	fmt.Println(string(b))
	outMu.Unlock()
}

func emitStatus(s string) { emit(event{Type: "status", State: s}) }
func emitDraft(t string)  { emit(event{Type: "draft", Text: t}) }
func emitCommit(t string) { emit(event{Type: "committed", Text: t}) }
func emitFinal(t string)  { emit(event{Type: "final", Text: t}) }
func emitInfo(t string)   { emit(event{Type: "info", Text: t}) }
func emitErr(t string)    { emit(event{Type: "error", Text: t}) }

// -------- Voxtral WebSocket message types --------

// client -> server
type vsSessionUpdate struct {
	Type    string            `json:"type"`
	Session vsSessionPayload  `json:"session"`
}
type vsSessionPayload struct {
	AudioFormat vsAudioFmt `json:"audio_format"`
}
type vsAudioFmt struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type vsAudioAppend struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

type vsAudioFlush struct {
	Type string `json:"type"` // "input_audio.flush"
}

type vsAudioEnd struct {
	Type string `json:"type"` // "input_audio.end"
}

// server -> client
type vsServerEvent struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Session struct {
		ID string `json:"id"`
	} `json:"session,omitempty"`
	Error struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// -------- Voxtral realtime session (Tier 1) --------

type voxtralConn struct {
	conn    *websocket.Conn
	audioQ  chan []byte // raw PCM chunks to send
	onDelta func(string)
	onDone  func(string)
}

func dialVoxtral(ctx context.Context, mistralKey string, onDelta, onDone func(string)) (*voxtralConn, error) {
	wsURL := fmt.Sprintf("%s?model=%s", voxtralWSURL, voxtralModel)

	hdr := http.Header{"Authorization": {"Bearer " + mistralKey}}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Configure audio format before sending audio.
	su := vsSessionUpdate{
		Type: "session.update",
		Session: vsSessionPayload{
			AudioFormat: vsAudioFmt{Encoding: "pcm_s16le", SampleRate: sampleRate},
		},
	}
	b, _ := json.Marshal(su)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("session.update: %w", err)
	}

	vc := &voxtralConn{
		conn:    conn,
		audioQ:  make(chan []byte, 256),
		onDelta: onDelta,
		onDone:  onDone,
	}
	go vc.reader(ctx)
	go vc.writer(ctx)
	return vc, nil
}

func (vc *voxtralConn) reader(ctx context.Context) {
	for {
		_, data, err := vc.conn.Read(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				fmt.Fprintf(os.Stderr, "voxtral reader: %v\n", err)
			}
			return
		}
		var evt vsServerEvent
		if err := json.Unmarshal(data, &evt); err != nil {
			// A frame we can't even parse means the protocol drifted. Fail loud.
			emitErr("voxtral: unparseable frame: " + err.Error())
			fmt.Fprintf(os.Stderr, "voxtral UNPARSEABLE frame: %v\nraw: %s\n", err, string(data))
			continue
		}
		// Known Voxtral realtime WS events (confirmed by probing the live API):
		//   session.created, session.updated, transcription.text.delta,
		//   transcription.done, error. Anything else means our mental model is
		//   wrong -> surface it loudly instead of silently swallowing it.
		switch evt.Type {
		case "session.created", "session.updated":
			fmt.Fprintf(os.Stderr, "voxtral: %s\n", evt.Type)
		case "transcription.text.delta":
			if evt.Text == "" {
				emitErr("voxtral: empty transcription.text.delta (unexpected)")
				fmt.Fprintf(os.Stderr, "voxtral UNEXPECTED empty delta: %s\n", string(data))
				break
			}
			vc.onDelta(evt.Text)
		case "transcription.done":
			fmt.Fprintf(os.Stderr, "voxtral: done %q\n", evt.Text)
			vc.onDone(evt.Text)
		case "error":
			emitErr("voxtral: " + evt.Error.Message)
			fmt.Fprintf(os.Stderr, "voxtral error frame: %s\n", string(data))
		default:
			emitErr("voxtral: UNKNOWN event type " + evt.Type)
			fmt.Fprintf(os.Stderr, "voxtral UNKNOWN EVENT (full frame): %s\n", string(data))
		}
	}
}

func (vc *voxtralConn) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-vc.audioQ:
			if !ok {
				return
			}
			// Split into chunks if needed (API limit: 262144 bytes decoded).
			for len(chunk) > 0 {
				size := voxtralChunkMax
				if size > len(chunk) {
					size = len(chunk)
				}
				msg := vsAudioAppend{
					Type:  "input_audio.append",
					Audio: base64.StdEncoding.EncodeToString(chunk[:size]),
				}
				b, _ := json.Marshal(msg)
				if err := vc.conn.Write(ctx, websocket.MessageText, b); err != nil {
					select {
					case <-ctx.Done():
					default:
						fmt.Fprintf(os.Stderr, "voxtral write: %v\n", err)
					}
					return
				}
				chunk = chunk[size:]
			}
		}
	}
}

func (vc *voxtralConn) send(chunk []byte) {
	cp := make([]byte, len(chunk))
	copy(cp, chunk)
	select {
	case vc.audioQ <- cp:
	default:
		// queue full; drop chunk (tier 2/3 still have the audio)
	}
}

func (vc *voxtralConn) close(ctx context.Context) {
	// Signal audio end then close.
	b, _ := json.Marshal(vsAudioEnd{Type: "input_audio.end"})
	_ = vc.conn.Write(ctx, websocket.MessageText, b)
	vc.conn.Close(websocket.StatusNormalClosure, "done")
}

// -------- Session --------

// session holds the state for one recording run.
type session struct {
	mu sync.Mutex

	// audio
	audioSinceCommit []byte

	// tier-1 voxtral draft accumulation (since last commit)
	voxtralAccum string

	// tier-3 committed text
	committedText string

	// voxtral connection (may be nil if API unavailable)
	vc *voxtralConn

	// in-flight flags to serialise refine and commit
	miniInflight   bool
	commitInflight bool

	// context for this session's lifetime
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	openaiKey  string
	mistralKey string
}

// onDelta is called from the voxtral reader goroutine.
func (s *session) onDelta(text string) {
	s.mu.Lock()
	s.voxtralAccum += text
	accum := s.voxtralAccum
	s.mu.Unlock()
	emitDraft(accum)
}

// onVoxtralDone is called when voxtral emits transcription.done
// (only expected on session end / input_audio.end).
func (s *session) onVoxtralDone(_ string) {
	// We don't use voxtral's done for committed text; gpt-4o handles that.
}

// refineLoop calls gpt-4o-mini on a ticker (tier 2).
func (s *session) refineLoop() {
	tick := time.NewTicker(refineInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.doRefine()
		}
	}
}

func (s *session) doRefine() {
	s.mu.Lock()
	if s.miniInflight || s.commitInflight {
		s.mu.Unlock()
		return
	}
	n := len(s.audioSinceCommit)
	if n < minAudioBytes {
		s.mu.Unlock()
		return
	}
	audio := make([]byte, n)
	copy(audio, s.audioSinceCommit)
	s.miniInflight = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.miniInflight = false
		s.mu.Unlock()
	}()

	wav, err := pcmToWAV(audio)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refine: wav: %v\n", err)
		return
	}
	secs := float64(n) / float64(bytesPerSec)
	fmt.Fprintf(os.Stderr, "refine: %s %.1fs...\n", draftModel, secs)
	t0 := time.Now()
	text, err := transcribeHTTP(s.openaiKey, draftModel, wav)
	fmt.Fprintf(os.Stderr, "refine: done %v %q\n", time.Since(t0).Round(time.Millisecond), text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refine: error: %v\n", err)
		return
	}

	// Mini replaces the voxtral rough draft with a cleaner version.
	// Voxtral continues and will append new incremental tokens on top.
	s.mu.Lock()
	s.voxtralAccum = text
	s.mu.Unlock()
	emitDraft(text)
}

// commitLoop calls gpt-4o-transcribe on a 30 s ticker (tier 3).
func (s *session) commitLoop() {
	tick := time.NewTicker(commitInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.doCommit(false)
		}
	}
}

// doCommit transcribes audio since last commit with the strong model.
// If final=true it is the stop-time tail commit.
func (s *session) doCommit(final bool) {
	s.mu.Lock()
	if s.commitInflight {
		s.mu.Unlock()
		return
	}
	snapLen := len(s.audioSinceCommit)
	if snapLen < minAudioBytes {
		s.mu.Unlock()
		if !final {
			fmt.Fprintln(os.Stderr, "commit: not enough audio, skipping")
		}
		return
	}
	audio := make([]byte, snapLen)
	copy(audio, s.audioSinceCommit)
	draftSnap := s.voxtralAccum // refined draft for this segment; fallback if strong is weak
	s.commitInflight = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.commitInflight = false
		s.mu.Unlock()
	}()

	wav, err := pcmToWAV(audio)
	if err != nil {
		fmt.Fprintf(os.Stderr, "commit: wav: %v\n", err)
		emitErr(fmt.Sprintf("commit pcmToWAV: %v", err))
		return
	}
	secs := float64(snapLen) / float64(bytesPerSec)
	label := "commit"
	if final {
		label = "final-commit"
	}
	fmt.Fprintf(os.Stderr, "%s: %s %.1fs...\n", label, commitModel, secs)
	t0 := time.Now()
	text, err := transcribeHTTP(s.openaiKey, commitModel, wav)
	fmt.Fprintf(os.Stderr, "%s: done %v %q\n", label, time.Since(t0).Round(time.Millisecond), text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: error: %v\n", label, err)
		emitErr(fmt.Sprintf("%s: %v", label, err))
		return
	}

	// Never lose spoken text. gpt-4o-transcribe sometimes returns empty or a
	// truncated result; if it's shorter than the refined draft we already have,
	// commit the draft instead so the orange text is never deleted.
	best := strings.TrimSpace(text)
	draft := strings.TrimSpace(draftSnap)
	if len([]rune(draft)) > len([]rune(best)) {
		if best == "" {
			fmt.Fprintf(os.Stderr, "%s: strong empty; committing refined draft\n", label)
		} else {
			fmt.Fprintf(os.Stderr, "%s: strong shorter than draft (%d<%d runes); committing draft\n",
				label, len([]rune(best)), len([]rune(draft)))
		}
		best = draft
	}
	if best == "" {
		// Nothing usable from either model: do NOT trim audio or reset the draft.
		// Leave the segment so the next commit (or the final commit) retries it.
		// This guarantees we never delete spoken text.
		fmt.Fprintln(os.Stderr, label+": no usable text; leaving segment for retry")
		return
	}

	s.mu.Lock()
	s.committedText = joinText(s.committedText, best)
	committed := s.committedText

	// Trim the committed audio from the front; preserve new audio that arrived
	// during the API call.
	if len(s.audioSinceCommit) > snapLen {
		tail := make([]byte, len(s.audioSinceCommit)-snapLen)
		copy(tail, s.audioSinceCommit[snapLen:])
		s.audioSinceCommit = tail
	} else {
		s.audioSinceCommit = nil
	}

	// Reset voxtral draft — new deltas will build the tail for the next window.
	s.voxtralAccum = ""
	s.mu.Unlock()

	emitCommit(committed)
	emitDraft("") // NON-OVERLAP: clear tail after commit
}

// finalize is called on stop: runs a final tier-3 call on remaining audio
// then emits the final event.
func (s *session) finalize() {
	emitStatus("transcribing")

	s.doCommit(true)

	s.mu.Lock()
	committed := s.committedText
	s.mu.Unlock()

	emitFinal(committed)
	emitStatus("idle")
}

// -------- Audio feed functions --------

func feedReplay(ctx context.Context, s *session, wavPath string) {
	f, err := os.Open(wavPath)
	if err != nil {
		emitErr(fmt.Sprintf("open %s: %v", wavPath, err))
		return
	}
	defer f.Close()

	pcm, err := readWAVPCM(f)
	if err != nil {
		emitErr(fmt.Sprintf("read WAV: %v", err))
		return
	}

	durSecs := float64(len(pcm)) / float64(bytesPerSec)
	fmt.Fprintf(os.Stderr, "replay: %.1fs (%d bytes) at 1x\n", durSecs, len(pcm))

	chunkDur := time.Duration(replayChunk) * time.Second / time.Duration(bytesPerSec)
	tick := time.NewTicker(chunkDur)
	defer tick.Stop()

	offset := 0
	for offset < len(pcm) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "replay: cancelled")
			return
		case <-tick.C:
		}
		end := offset + replayChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[offset:end]

		s.mu.Lock()
		s.audioSinceCommit = append(s.audioSinceCommit, chunk...)
		s.mu.Unlock()

		if s.vc != nil {
			s.vc.send(chunk)
		}
		offset = end
	}
	fmt.Fprintln(os.Stderr, "replay: all audio fed")
}

func feedMic(ctx context.Context, s *session) {
	cmd := exec.CommandContext(ctx, "pw-record",
		"--format", "s16", "--rate", "16000", "--channels", "1", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emitErr(fmt.Sprintf("mic pipe: %v", err))
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		emitErr(fmt.Sprintf("mic start: %v", err))
		return
	}

	buf := make([]byte, replayChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			s.mu.Lock()
			s.audioSinceCommit = append(s.audioSinceCommit, chunk...)
			s.mu.Unlock()
			if s.vc != nil {
				s.vc.send(chunk)
			}
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	fmt.Fprintln(os.Stderr, "mic: stopped")
}

// -------- Session lifecycle --------

var (
	sessMu  sync.Mutex
	curSess *session
)

func startSession(openaiKey, mistralKey string, feedFn func(context.Context, *session)) {
	// Cancel any existing session.
	sessMu.Lock()
	old := curSess
	sessMu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}

	emit(event{Type: "reset"})

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		openaiKey:  openaiKey,
		mistralKey: mistralKey,
	}

	sessMu.Lock()
	curSess = s
	sessMu.Unlock()

	emitStatus("recording")

	go func() {
		defer close(s.done)

		// Tier 1: open voxtral realtime session.
		vc, err := dialVoxtral(ctx, mistralKey, s.onDelta, s.onVoxtralDone)
		if err != nil {
			fmt.Fprintf(os.Stderr, "voxtral: unavailable (%v) — tiers 2/3 only\n", err)
			emitInfo("voxtral unavailable; showing refined drafts only")
		} else {
			s.mu.Lock()
			s.vc = vc
			s.mu.Unlock()
			fmt.Fprintln(os.Stderr, "voxtral: tier 1 active")
		}

		// Tier 2 refinement and tier 3 commit run in background.
		go s.refineLoop()
		go s.commitLoop()

		// Feed audio until done or cancelled.
		feedFn(ctx, s)
		fmt.Fprintln(os.Stderr, "feed: done")
	}()
}

func stopSession(openaiKey string) {
	sessMu.Lock()
	s := curSess
	sessMu.Unlock()

	if s == nil {
		emitErr("no active session")
		return
	}

	// Stop feed and background tickers.
	s.cancel()
	<-s.done

	// Close voxtral connection.
	s.mu.Lock()
	vc := s.vc
	s.mu.Unlock()
	if vc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		vc.close(ctx)
	}

	// Tier 3 final commit + emit final event.
	s.finalize()

	sessMu.Lock()
	curSess = nil
	sessMu.Unlock()
}

// -------- HTTP transcription (tiers 2 and 3) --------

func transcribeHTTP(key, model string, wavData []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	_ = mw.WriteField("model", model)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wavData); err != nil {
		return "", err
	}
	_ = mw.Close()

	// Hard timeout: a context-less DefaultClient call can hang forever, which
	// previously wedged the commit loop and deadlocked stop/finalize.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, rb)
	}

	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rb, &res); err != nil {
		return "", fmt.Errorf("parse: %w (body: %s)", err, rb)
	}
	return res.Text, nil
}

// -------- Audio utilities --------

func pcmToWAV(pcm []byte) ([]byte, error) {
	var buf bytes.Buffer
	const (
		sr        = 16000
		ch        = 1
		bps       = 16
		byteRate  = sr * ch * bps / 8
		blockAlign = ch * bps / 8
	)
	dataLen := uint32(len(pcm))
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(ch))
	binary.Write(&buf, binary.LittleEndian, uint32(sr))
	binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&buf, binary.LittleEndian, uint16(bps))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataLen)
	buf.Write(pcm)
	return buf.Bytes(), nil
}

func readWAVPCM(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	for i := 0; i+8 <= len(data); i++ {
		if data[i] == 'd' && data[i+1] == 'a' && data[i+2] == 't' && data[i+3] == 'a' {
			n := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
			start := i + 8
			end := start + n
			if end > len(data) {
				end = len(data)
			}
			return data[start:end], nil
		}
	}
	return nil, fmt.Errorf("no WAV data chunk")
}

// joinText concatenates two transcript strings with proper spacing.
func joinText(a, b string) string {
	a = strings.TrimRight(a, " ")
	b = strings.TrimLeft(b, " ")
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " " + b
}

// -------- Control socket --------

func handleControl(conn net.Conn, openaiKey, mistralKey string) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		cmd := parts[0]

		switch cmd {
		case "replay":
			path := "../sample.wav"
			if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
				path = strings.TrimSpace(parts[1])
			}
			abs, _ := filepath.Abs(path)
			fmt.Fprintf(conn, "replaying %s\n", abs)
			go startSession(openaiKey, mistralKey, func(ctx context.Context, s *session) {
				feedReplay(ctx, s, abs)
			})

		case "mic":
			fmt.Fprintln(conn, "starting mic")
			go startSession(openaiKey, mistralKey, func(ctx context.Context, s *session) {
				feedMic(ctx, s)
			})

		case "stop":
			fmt.Fprintln(conn, "stopping and finalizing...")
			go stopSession(openaiKey)

		case "status":
			sessMu.Lock()
			active := curSess != nil
			sessMu.Unlock()
			if active {
				fmt.Fprintln(conn, "active")
			} else {
				fmt.Fprintln(conn, "idle")
			}

		case "quit":
			fmt.Fprintln(conn, "bye")
			os.Exit(0)

		default:
			fmt.Fprintf(conn, "unknown command: %s\n", cmd)
		}
	}
}

// -------- main --------

func main() {
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}
	mistralKey := os.Getenv("MISTRAL_API_KEY")
	if mistralKey == "" {
		fmt.Fprintln(os.Stderr, "MISTRAL_API_KEY not set")
		os.Exit(1)
	}

	sockPath, _ := filepath.Abs("control.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "control socket: %s\n", sockPath)
	fmt.Fprintf(os.Stderr, "tiers: voxtral-realtime | %s every %s | %s every %s\n",
		draftModel, refineInterval, commitModel, commitInterval)

	emitStatus("idle")

	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(c, openaiKey, mistralKey)
	}
}
