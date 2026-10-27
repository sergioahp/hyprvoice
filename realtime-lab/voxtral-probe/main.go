// voxtral-probe connects to the Mistral Voxtral realtime WS, streams a WAV at
// realtime, periodically flushes, and dumps EVERY received frame verbatim to
// stdout (one JSON object per line) so we can see the full protocol -- including
// whatever carries the volatile "head" and any event types our clients ignore.
//
// stdout: raw frames (for jq). stderr: a timestamped send/recv timeline.
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

const (
	wsURL = "wss://api.mistral.ai/v1/audio/transcriptions/realtime"
	model = "voxtral-mini-transcribe-realtime-2602"
)

func main() {
	wav := flag.String("wav", "../sample.wav", "16kHz mono s16 WAV to stream")
	flushMs := flag.Int("flush", 400, "send input_audio.flush every N ms (0=never)")
	delayMs := flag.Int("delay", 480, "target_streaming_delay_ms in session.update (-1=omit)")
	flag.Parse()

	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "MISTRAL_API_KEY not set")
		os.Exit(1)
	}
	raw, err := os.ReadFile(*wav)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read wav:", err)
		os.Exit(1)
	}
	pcm := wavData(raw)
	fmt.Fprintf(os.Stderr, "pcm: %d bytes (%.1fs @16k)\n", len(pcm), float64(len(pcm))/32000.0)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t0 := time.Now()
	ts := func() string { return fmt.Sprintf("+%6dms", time.Since(t0).Milliseconds()) }

	conn, _, err := websocket.Dial(ctx, wsURL+"?model="+model, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + key}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(8 << 20)

	send := func(label, js string) {
		fmt.Fprintf(os.Stderr, "%s SEND %s\n", ts(), label)
		_ = conn.Write(ctx, websocket.MessageText, []byte(js))
	}

	// session.update
	delay := ""
	if *delayMs >= 0 {
		delay = fmt.Sprintf(`,"target_streaming_delay_ms":%d`, *delayMs)
	}
	send("session.update", fmt.Sprintf(
		`{"type":"session.update","session":{"audio_format":{"encoding":"pcm_s16le","sample_rate":16000}%s}}`, delay))

	// reader: dump every frame verbatim to stdout, timeline to stderr
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s RECV closed: %v\n", ts(), err)
				return
			}
			fmt.Println(string(data)) // raw frame for jq
			fmt.Fprintf(os.Stderr, "%s RECV %s\n", ts(), peekType(data))
		}
	}()

	// writer: stream PCM at realtime in 20ms (640B) chunks; flush periodically
	const chunk = 640
	var lastFlush time.Time
	for off := 0; off < len(pcm); off += chunk {
		end := off + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		b64 := base64.StdEncoding.EncodeToString(pcm[off:end])
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input_audio.append","audio":"`+b64+`"}`))
		if *flushMs > 0 && time.Since(lastFlush) >= time.Duration(*flushMs)*time.Millisecond {
			send("input_audio.flush", `{"type":"input_audio.flush"}`)
			lastFlush = time.Now()
		}
		time.Sleep(20 * time.Millisecond)
	}
	send("input_audio.end", `{"type":"input_audio.end"}`)

	// drain trailing frames
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		fmt.Fprintf(os.Stderr, "%s drain timeout\n", ts())
	}
}

func peekType(b []byte) string {
	// cheap "type" extraction without full unmarshal
	s := string(b)
	i := indexOf(s, `"type"`)
	if i < 0 {
		return "(no type) " + truncate(s, 80)
	}
	return truncate(s[i:], 100)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// wavData returns the PCM payload of a WAV by scanning for the data chunk.
func wavData(b []byte) []byte {
	for i := 0; i+8 < len(b); i++ {
		if string(b[i:i+4]) == "data" {
			n := int(binary.LittleEndian.Uint32(b[i+4:]))
			start := i + 8
			if start+n > len(b) {
				n = len(b) - start
			}
			return b[start : start+n]
		}
	}
	return b
}
