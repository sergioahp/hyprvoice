// idea-b: cheap polling draft via gpt-4o-mini-transcribe + one strong final
//
// Draft path: on a ~1 Hz timer, POST the last TAIL_SECS of PCM audio to
// gpt-4o-mini-transcribe and emit the result as a "draft" event. Only one
// draft request is in flight at a time; ticks are skipped if the previous
// call has not returned yet.
//
// Final path: on "stop", POST the full audio buffer to gpt-4o-transcribe
// (called exactly once) and emit the result as a "final" event.
//
// Window strategy: cap draft audio at TAIL_SECS (8 s). For recordings
// longer than TAIL_SECS the draft reflects only recent speech. The strong
// final always gets the full buffer regardless of length.
//
// All logs and the control-socket path go to stderr. Only JSONL events on stdout.
package main

import (
	"bufio"
	"bytes"
	"context"
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
	"sync/atomic"
	"time"
)

// -------- constants --------

const (
	sampleRate    = 16000
	bytesPerSample = 2 // s16le mono
	bytesPerSec   = sampleRate * bytesPerSample // 32 000

	// Draft poll: at most 1 Hz; use 900 ms to stay safely under the limit.
	pollInterval = 900 * time.Millisecond

	// Max audio sent to mini-transcribe per draft call (8 s).
	tailSecs  = 8.0
	tailBytes = int(tailSecs * float64(bytesPerSec)) // 256 000

	draftModel = "gpt-4o-mini-transcribe"
	finalModel = "gpt-4o-transcribe"

	// Replay chunk: 100 ms of audio at a time.
	replayChunk = bytesPerSec / 10 // 3 200 bytes
)

// -------- event stream --------

type event struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
}

var (
	outMu  sync.Mutex
	statMu sync.Mutex
	curSt  = "idle"
)

func emit(e event) {
	outMu.Lock()
	defer outMu.Unlock()
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

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

// -------- session --------

// session holds the audio buffer for one recording/replay run.
type session struct {
	sync.Mutex
	audioBuf []byte
	cancel   context.CancelFunc
	done     chan struct{} // closed when the feed goroutine exits
}

var (
	sessMu sync.Mutex
	sess   *session

	inflight atomic.Bool
)

// -------- main --------

var apiKey string

func main() {
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

	setState("idle")
	go pollLoop()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(conn)
	}
}

// -------- poll loop --------

func pollLoop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		if getState() != "recording" {
			continue
		}
		if inflight.Load() {
			fmt.Fprintln(os.Stderr, "poll: skip (inflight)")
			continue
		}

		sessMu.Lock()
		s := sess
		sessMu.Unlock()
		if s == nil {
			continue
		}

		s.Lock()
		bufLen := len(s.audioBuf)
		// Need at least 250 ms before first poll
		if bufLen < bytesPerSec/4 {
			s.Unlock()
			continue
		}
		// Rolling tail window: only send the last TAIL_SECS of audio
		start := 0
		if bufLen > tailBytes {
			start = bufLen - tailBytes
		}
		audioSlice := make([]byte, bufLen-start)
		copy(audioSlice, s.audioBuf[start:])
		s.Unlock()

		go func(audio []byte) {
			if !inflight.CompareAndSwap(false, true) {
				return
			}
			defer inflight.Store(false)

			wav, err := pcmToWAV(audio)
			if err != nil {
				fmt.Fprintf(os.Stderr, "poll: pcmToWAV: %v\n", err)
				return
			}

			t0 := time.Now()
			text, err := transcribeHTTP(apiKey, draftModel, wav)
			elapsed := time.Since(t0)
			if err != nil {
				fmt.Fprintf(os.Stderr, "poll: draft error (%v): %v\n", elapsed, err)
				return
			}
			durSecs := float64(len(audio)) / float64(bytesPerSec)
			fmt.Fprintf(os.Stderr, "poll: draft in %v (%.1fs audio): %q\n",
				elapsed.Round(time.Millisecond), durSecs, text)

			emit(event{Type: "draft", Text: text})
		}(audioSlice)
	}
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
			fmt.Fprintln(conn, "stopping and finalizing...")
			go doFinalize()

		case "status":
			sessMu.Lock()
			var bufLen int
			if sess != nil {
				sess.Lock()
				bufLen = len(sess.audioBuf)
				sess.Unlock()
			}
			sessMu.Unlock()
			fmt.Fprintf(conn, "state=%s buf=%.1fs\n",
				getState(), float64(bufLen)/float64(bytesPerSec))

		case "quit":
			fmt.Fprintln(conn, "bye")
			os.Exit(0)

		default:
			fmt.Fprintf(conn, "unknown: %s\n", cmd)
		}
	}
}

// startFeed cancels any active feed, resets state, then runs feedFn in a goroutine.
func startFeed(feedFn func(context.Context, *session)) {
	sessMu.Lock()
	old := sess
	sessMu.Unlock()

	if old != nil {
		old.cancel()
		<-old.done
	}

	emit(event{Type: "reset"})

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	sessMu.Lock()
	sess = s
	sessMu.Unlock()

	setState("recording")

	go func() {
		defer close(s.done)
		feedFn(ctx, s)
		// Feed completed naturally (replay EOF) or was cancelled.
		// Only change state if we're still in "recording" — doFinalize may have
		// already moved us to "transcribing".
		if getState() == "recording" {
			setState("idle")
			fmt.Fprintln(os.Stderr, "feed: done — send 'stop' for final transcript")
		}
	}()
}

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
	fmt.Fprintf(os.Stderr, "replay: %d bytes (%.1fs) paced at 1x\n", len(pcm), durSecs)

	chunkDur := time.Duration(replayChunk) * time.Second / time.Duration(bytesPerSec)
	ticker := time.NewTicker(chunkDur)
	defer ticker.Stop()

	offset := 0
	for offset < len(pcm) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "replay: cancelled")
			return
		case <-ticker.C:
		}
		end := offset + replayChunk
		if end > len(pcm) {
			end = len(pcm)
		}
		s.Lock()
		s.audioBuf = append(s.audioBuf, pcm[offset:end]...)
		s.Unlock()
		offset = end
	}
	fmt.Fprintln(os.Stderr, "replay: finished feeding all audio")
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
			s.Lock()
			s.audioBuf = append(s.audioBuf, buf[:n]...)
			s.Unlock()
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	fmt.Fprintln(os.Stderr, "mic: stopped")
}

// doFinalize stops the active feed and runs the strong model on the full buffer.
func doFinalize() {
	sessMu.Lock()
	s := sess
	sessMu.Unlock()

	if s == nil {
		emit(event{Type: "error", Text: "no audio session to finalize"})
		return
	}

	// Stop the feed goroutine and wait for it.
	s.cancel()
	<-s.done

	// Drain any inflight draft request before emitting final.
	for inflight.Load() {
		time.Sleep(20 * time.Millisecond)
	}

	s.Lock()
	buf := make([]byte, len(s.audioBuf))
	copy(buf, s.audioBuf)
	s.Unlock()

	if len(buf) == 0 {
		emit(event{Type: "error", Text: "no audio captured"})
		setState("idle")
		return
	}

	setState("transcribing")

	wav, err := pcmToWAV(buf)
	if err != nil {
		emit(event{Type: "error", Text: fmt.Sprintf("pcmToWAV: %v", err)})
		setState("idle")
		return
	}

	durSecs := float64(len(buf)) / float64(bytesPerSec)
	fmt.Fprintf(os.Stderr, "final: calling %s on %.1fs of audio...\n", finalModel, durSecs)

	t0 := time.Now()
	text, err := transcribeHTTP(apiKey, finalModel, wav)
	elapsed := time.Since(t0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "final: error (%v): %v\n", elapsed, err)
		emit(event{Type: "error", Text: fmt.Sprintf("final transcription: %v", err)})
		setState("idle")
		return
	}
	fmt.Fprintf(os.Stderr, "final: done in %v: %q\n", elapsed, text)

	emit(event{Type: "final", Text: text})
	setState("idle")

	sessMu.Lock()
	sess = nil
	sessMu.Unlock()
}

// -------- HTTP transcription --------

func transcribeHTTP(key, model string, wavData []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	if err := mw.WriteField("model", model); err != nil {
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

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", &body)
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

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

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

// -------- audio helpers --------

func pcmToWAV(pcm []byte) ([]byte, error) {
	var buf bytes.Buffer

	const sr = 16000
	const ch = 1
	const bps = 16
	const byteRate = sr * ch * bps / 8
	const blockAlign = ch * bps / 8

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

// readWAVPCM scans r for a WAV "data" chunk and returns the raw PCM bytes.
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
	return nil, fmt.Errorf("no data chunk found in WAV")
}
