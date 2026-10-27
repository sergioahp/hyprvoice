// idea-c: VAD-segmented pipeline with progressive strong lock-in.
//
// Voice-activity detection (energy-based RMS threshold + minimum silence
// duration) splits the incoming PCM stream into speech segments.  Each
// CLOSED segment is sent to gpt-4o-transcribe (strong, rate-limited to
// >= 1 s spacing) and its result is appended to committed text.  While a
// segment is OPEN, gpt-4o-mini-transcribe is polled every ~1 s on just
// that segment's audio to produce a volatile draft.
//
// At any moment the visualizer shows:
//   committed (stable, strong-model)  +  draft (volatile, current segment only)
//
// On "stop": the last open segment is finalized, then "final" is emitted.
// End-latency is near-zero when most segments were already committed.
//
// All logs and diagnostics go to stderr.  Stdout carries ONLY JSONL events.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// -------- audio constants --------

const (
	sampleRate    = 16000
	bytesPerSample = 2             // s16le mono
	bytesPerSec   = sampleRate * bytesPerSample // 32_000

	// Replay: push 100 ms of audio per tick.
	replayChunk = bytesPerSec / 10 // 3_200 bytes
)

// -------- VAD / pipeline tunable defaults --------

const (
	// RMS below this = silence (normalized to [0,1]).
	// 0.015 works well for clean mic/TTS audio; raise to 0.03-0.05 for noisy rooms.
	defaultRMSThreshold = 0.015

	// Continuous silence >= this closes the current segment.
	defaultMinSilence = 600 * time.Millisecond

	// How often to call the draft (mini) model on the open segment.
	defaultDraftPoll = 1000 * time.Millisecond

	// Do not draft until at least 250 ms of speech is accumulated.
	minDraftPCM = bytesPerSec / 4

	draftModel = "gpt-4o-mini-transcribe"
	strongModel = "gpt-4o-transcribe"
)

// -------- event stream (stdout, JSONL) --------

type event struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
}

var outMu sync.Mutex

func emit(e event) {
	outMu.Lock()
	defer outMu.Unlock()
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

var (
	statMu sync.Mutex
	curSt  = "idle"
)

func setState(s string) {
	statMu.Lock()
	curSt = s
	statMu.Unlock()
	emit(event{Type: "status", State: s})
}

func getState() string {
	statMu.Lock()
	defer statMu.Unlock()
	return curSt
}

// -------- global API key --------

var apiKey string

// -------- session --------

// session holds all goroutines and shared state for one recording/replay run.
type session struct {
	ctx    context.Context
	cancel context.CancelFunc

	// audioCh receives raw PCM chunks from the feed; closed when feed goroutine exits.
	audioCh chan []byte

	// segCh receives complete segment PCM (closed segments); closed when vadLoop exits.
	segCh chan []byte

	// draftBuf holds PCM of the currently open segment, updated by vadLoop, read by draftPoller.
	mu        sync.RWMutex
	draftBuf  []byte
	committed string // all strong-model results concatenated
	segCount  int    // number of segments successfully finalized

	// segID is incremented each time a segment closes; lets draftPoller detect
	// stale API calls that spanned a segment boundary.
	segID atomic.Int32

	// Closed when strongFinalizer goroutine exits.
	finalizerDone chan struct{}

	// VAD knobs for this session.
	rmsThreshold float64
	minSilence   time.Duration
	draftPoll    time.Duration
}

func newSession(rmsThreshold float64, minSilence, draftPoll time.Duration) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		ctx:           ctx,
		cancel:        cancel,
		audioCh:       make(chan []byte, 64),
		segCh:         make(chan []byte, 128), // generous buffer; VAD never blocks
		finalizerDone: make(chan struct{}),
		rmsThreshold:  rmsThreshold,
		minSilence:    minSilence,
		draftPoll:     draftPoll,
	}
}

// start launches the three pipeline goroutines.
func (s *session) start() {
	go s.vadLoop()
	go s.draftPoller()
	go s.strongFinalizer()
}

// -------- energy-based VAD --------

// computeRMS returns the root mean square of s16le PCM samples, normalized to [0,1].
func computeRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:i+2]))) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

func (s *session) vadLoop() {
	defer close(s.segCh)

	var (
		segBuf       []byte
		inSpeech     bool
		silenceStart time.Time
	)

	// closeSegment finalizes the current segment and sends it to the strong queue.
	closeSegment := func(reason string) {
		if len(segBuf) == 0 {
			return
		}
		seg := make([]byte, len(segBuf))
		copy(seg, segBuf)
		segBuf = nil

		// Clear draftBuf and bump segID so any in-flight draft API call
		// can detect it is now stale and discard its result.
		s.mu.Lock()
		s.draftBuf = nil
		s.mu.Unlock()
		s.segID.Add(1)

		dur := float64(len(seg)) / float64(bytesPerSec)
		log.Printf("VAD: segment closed (%s, %.2fs → strong queue)", reason, dur)
		s.segCh <- seg // never blocks; segCh is buffered
	}

	for chunk := range s.audioCh {
		rms := computeRMS(chunk)

		if rms >= s.rmsThreshold {
			// Speech energy.
			if !inSpeech {
				log.Printf("VAD: speech start  rms=%.4f", rms)
				inSpeech = true
			}
			silenceStart = time.Time{} // reset silence timer
			segBuf = append(segBuf, chunk...)

			s.mu.Lock()
			s.draftBuf = make([]byte, len(segBuf))
			copy(s.draftBuf, segBuf)
			s.mu.Unlock()

		} else if inSpeech {
			// Below threshold while in a speech segment — could be within-word gap.
			segBuf = append(segBuf, chunk...) // accumulate silence into segment

			if silenceStart.IsZero() {
				silenceStart = time.Now()
			}
			if time.Since(silenceStart) >= s.minSilence {
				log.Printf("VAD: silence >= %v → close segment", s.minSilence)
				inSpeech = false
				closeSegment("silence")
				silenceStart = time.Time{}
				emit(event{Type: "info", Text: "segment committed"})
			}
		}
	}

	// audioCh was closed (feed done) — finalize any remaining open segment.
	if inSpeech || len(segBuf) > 0 {
		log.Printf("VAD: feed ended with %.2fs open — finalizing",
			float64(len(segBuf))/float64(bytesPerSec))
		closeSegment("feed-end")
	}
	log.Printf("VAD: loop exited")
}

// -------- draft poller (gpt-4o-mini-transcribe, ~1 Hz) --------

func (s *session) draftPoller() {
	ticker := time.NewTicker(s.draftPoll)
	defer ticker.Stop()

	var lastPolledLen int
	var lastSegID int32 = -1

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}

		// When the segment ID changes, reset state so we poll the new segment fresh.
		curSegID := s.segID.Load()
		if curSegID != lastSegID {
			lastSegID = curSegID
			lastPolledLen = 0
		}

		s.mu.RLock()
		dbLen := len(s.draftBuf)
		var buf []byte
		if dbLen >= minDraftPCM && dbLen != lastPolledLen {
			buf = make([]byte, dbLen)
			copy(buf, s.draftBuf)
		}
		s.mu.RUnlock()

		if buf == nil {
			continue
		}
		lastPolledLen = dbLen

		// Snapshot segment ID before the API call to detect boundary crossings.
		preID := s.segID.Load()

		ctx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
		wav, _ := pcmToWAV(buf)
		t0 := time.Now()
		text, err := transcribeHTTP(ctx, apiKey, draftModel, wav)
		cancel()
		if err != nil {
			log.Printf("draft: error (%v): %v", time.Since(t0).Round(time.Millisecond), err)
			continue
		}
		dur := float64(len(buf)) / float64(bytesPerSec)
		log.Printf("draft (%.1fs, %v): %q", dur, time.Since(t0).Round(time.Millisecond), text)

		// Discard the result if the segment closed while the API call was in flight.
		if s.segID.Load() != preID {
			log.Printf("draft: stale — segment boundary crossed during API call, discarding")
			lastPolledLen = 0 // force fresh poll on next tick
			continue
		}
		emit(event{Type: "draft", Text: strings.TrimSpace(text)})
	}
}

// -------- strong finalizer (gpt-4o-transcribe, rate-limited >= 1 s) --------

func (s *session) strongFinalizer() {
	defer close(s.finalizerDone)

	var lastCallTime time.Time

	for seg := range s.segCh {
		// Enforce >= 1 s spacing between strong model calls.
		if !lastCallTime.IsZero() {
			wait := time.Second - time.Since(lastCallTime)
			if wait > 0 {
				log.Printf("strong: rate-limit sleep %v", wait.Round(time.Millisecond))
				time.Sleep(wait)
			}
		}

		dur := float64(len(seg)) / float64(bytesPerSec)
		log.Printf("strong: calling %s on %.2fs segment", strongModel, dur)

		wav, _ := pcmToWAV(seg)
		// Use Background so finalization completes even after session ctx is cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t0 := time.Now()
		text, err := transcribeHTTP(ctx, apiKey, strongModel, wav)
		cancel()
		lastCallTime = time.Now()

		if err != nil {
			log.Printf("strong: error (%v): %v", time.Since(t0).Round(time.Millisecond), err)
			emit(event{Type: "error", Text: fmt.Sprintf("strong transcription: %v", err)})
			continue
		}
		text = strings.TrimSpace(text)
		log.Printf("strong: done in %v (%.2fs audio): %q",
			time.Since(t0).Round(time.Millisecond), dur, text)

		if text == "" {
			continue
		}

		s.mu.Lock()
		if s.committed != "" {
			s.committed += " " + text
		} else {
			s.committed = text
		}
		s.segCount++
		committed := s.committed
		s.mu.Unlock()

		emit(event{Type: "committed", Text: committed})
		emit(event{Type: "draft", Text: ""}) // clear draft tail; segment is now locked in
	}
	log.Printf("strong: finalizer done (%d segments)", s.segCount)
}

// -------- session management --------

var (
	sessMu  sync.Mutex
	curSess *session
	startMu sync.Mutex // serializes concurrent replay/mic commands
)

// startFeed cancels any existing session, resets the visualizer, and starts a new one.
func startFeed(feedFn func(ctx context.Context, s *session)) {
	startMu.Lock()
	defer startMu.Unlock()

	sessMu.Lock()
	old := curSess
	sessMu.Unlock()

	if old != nil {
		old.cancel()
		<-old.finalizerDone
	}

	emit(event{Type: "reset"})

	s := newSession(defaultRMSThreshold, defaultMinSilence, defaultDraftPoll)

	sessMu.Lock()
	curSess = s
	sessMu.Unlock()

	s.start()
	setState("recording")

	go func() {
		feedFn(s.ctx, s)
		// Close audioCh so vadLoop knows no more audio is coming.
		close(s.audioCh)
		// Wait for the full pipeline to drain.
		<-s.finalizerDone
		// Only revert to idle if nothing else took over (stop sets "transcribing").
		if getState() == "recording" {
			setState("idle")
			log.Printf("feed: finished — type 'stop' to emit final transcript")
		}
	}()
}

// -------- audio feed sources --------

func feedReplay(ctx context.Context, s *session, wavPath string) {
	f, err := os.Open(wavPath)
	if err != nil {
		emit(event{Type: "error", Text: fmt.Sprintf("open %s: %v", wavPath, err)})
		return
	}
	defer f.Close()

	pcm, err := readWAVPCM(f)
	if err != nil {
		emit(event{Type: "error", Text: fmt.Sprintf("read WAV: %v", err)})
		return
	}

	durSecs := float64(len(pcm)) / float64(bytesPerSec)
	log.Printf("replay: %.1fs audio, pacing at 1x realtime", durSecs)

	chunkDur := time.Duration(replayChunk) * time.Second / time.Duration(bytesPerSec)
	ticker := time.NewTicker(chunkDur)
	defer ticker.Stop()

	offset := 0
	for offset < len(pcm) {
		select {
		case <-ctx.Done():
			log.Printf("replay: cancelled at %.1fs / %.1fs",
				float64(offset)/float64(bytesPerSec), durSecs)
			return
		case <-ticker.C:
		}
		end := offset + replayChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := make([]byte, end-offset)
		copy(chunk, pcm[offset:end])
		select {
		case s.audioCh <- chunk:
		case <-ctx.Done():
			return
		}
		offset = end
	}
	log.Printf("replay: finished feeding %.1fs of audio", durSecs)
}

func feedMic(ctx context.Context, s *session) {
	cmd := exec.CommandContext(ctx, "pw-record",
		"--format", "s16", "--rate", "16000", "--channels", "1", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(event{Type: "error", Text: fmt.Sprintf("mic pipe: %v", err)})
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		emit(event{Type: "error", Text: fmt.Sprintf("mic start: %v", err)})
		return
	}

	buf := make([]byte, replayChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.audioCh <- chunk:
			case <-ctx.Done():
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return
			}
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	log.Printf("mic: stopped")
}

// -------- stop / finalize --------

func doStop() {
	sessMu.Lock()
	s := curSess
	sessMu.Unlock()

	if s == nil {
		emit(event{Type: "error", Text: "no active session to stop"})
		return
	}

	setState("transcribing")

	// Cancel context → feed goroutine exits → audioCh closes → vadLoop sends
	// last segment → segCh closes → finalizer drains → finalizerDone closes.
	s.cancel()
	<-s.finalizerDone

	s.mu.RLock()
	committed := s.committed
	s.mu.RUnlock()

	emit(event{Type: "final", Text: committed})
	setState("idle")

	sessMu.Lock()
	curSess = nil
	sessMu.Unlock()
}

// -------- control socket --------

func handleControl(conn net.Conn) {
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
			go startFeed(func(ctx context.Context, s *session) {
				feedReplay(ctx, s, abs)
			})

		case "mic":
			fmt.Fprintln(conn, "starting mic")
			go startFeed(func(ctx context.Context, s *session) {
				feedMic(ctx, s)
			})

		case "stop":
			fmt.Fprintln(conn, "finalizing...")
			go doStop()

		case "status":
			sessMu.Lock()
			var openSecs float64
			var committed string
			var segCount int
			if curSess != nil {
				curSess.mu.RLock()
				openSecs = float64(len(curSess.draftBuf)) / float64(bytesPerSec)
				committed = curSess.committed
				segCount = curSess.segCount
				curSess.mu.RUnlock()
			}
			sessMu.Unlock()
			fmt.Fprintf(conn, "state=%s segments=%d open=%.2fs committed=%q\n",
				getState(), segCount, openSecs, committed)

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
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	apiKey = os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	sockPath, _ := filepath.Abs("control.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot listen on %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "control socket: %s\n", sockPath)
	fmt.Fprintf(os.Stderr, "VAD knobs: rms_threshold=%.3f min_silence=%v draft_poll=%v\n",
		defaultRMSThreshold, defaultMinSilence, defaultDraftPoll)

	setState("idle")

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(conn)
	}
}

// -------- HTTP transcription (OpenAI audio/transcriptions) --------

func transcribeHTTP(ctx context.Context, key, model string, wavData []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if err := mw.WriteField("model", model); err != nil {
		return "", err
	}
	if err := mw.WriteField("language", "en"); err != nil {
		return "", err
	}

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
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.openai.com/v1/audio/transcriptions", &body)
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
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(rb))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rb, &result); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, string(rb))
	}
	return result.Text, nil
}

// -------- WAV helpers --------

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

// readWAVPCM scans r for the WAV "data" chunk and returns raw PCM bytes.
func readWAVPCM(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	for i := 0; i+8 <= len(data); i++ {
		if data[i] == 'd' && data[i+1] == 'a' && data[i+2] == 't' && data[i+3] == 'a' {
			chunkSize := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
			start := i + 8
			end := start + chunkSize
			if end > len(data) {
				end = len(data)
			}
			return data[start:end], nil
		}
	}
	return nil, fmt.Errorf("no 'data' chunk found in WAV file")
}
