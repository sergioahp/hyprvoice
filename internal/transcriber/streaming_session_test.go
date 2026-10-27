package transcriber

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/leonardotrapani/hyprvoice/internal/provider"
	"github.com/leonardotrapani/hyprvoice/internal/recording"
)

func streamingTestServer(t *testing.T, handler func(*websocket.Conn, *http.Request)) (*httptest.Server, *provider.EndpointConfig) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn, r)
	}))
	t.Cleanup(server.Close)
	return server, &provider.EndpointConfig{BaseURL: "ws" + strings.TrimPrefix(server.URL, "http")}
}

func TestStreamingDisconnectFailsWithoutLosingEarlierAudio(t *testing.T) {
	for _, name := range []string{"deepgram", "openai", "elevenlabs"} {
		t.Run(name, func(t *testing.T) {
			var connections atomic.Int32
			_, endpoint := streamingTestServer(t, func(conn *websocket.Conn, _ *http.Request) {
				connections.Add(1)
				if name == "openai" {
					conn.ReadMessage()
				}
				conn.ReadMessage() // The server has received uncommitted audio.
			})
			var adapter StreamingAdapter
			switch name {
			case "deepgram":
				adapter = NewDeepgramAdapter(endpoint, "key", "nova-3", "en", nil)
			case "openai":
				adapter = NewOpenAIRealtimeAdapter(endpoint, "key", "gpt-4o-realtime-preview", "en", nil)
			case "elevenlabs":
				adapter = NewElevenLabsStreamingAdapter(endpoint, "key", "scribe_v1", "en", nil)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := adapter.Start(ctx, "en"); err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			if err := adapter.SendChunk(make([]byte, 320)); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-adapter.Results():
				if !IsFatalTranscriptionError(result.Error) {
					t.Fatalf("disconnect error=%v", result.Error)
				}
			case <-ctx.Done():
				t.Fatal("disconnect did not fail session")
			}
			if connections.Load() != 1 {
				t.Fatalf("session was silently reconnected %d times", connections.Load())
			}
		})
	}
}

func TestDeepgramFinalizeWaitsForAllSegments(t *testing.T) {
	first, release := make(chan struct{}), make(chan struct{})
	_, endpoint := streamingTestServer(t, func(conn *websocket.Conn, _ *http.Request) {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var request deepgramCloseStream
		json.Unmarshal(message, &request)
		if request.Type != "CloseStream" {
			t.Errorf("request=%s", message)
			return
		}
		send := func(text string) {
			conn.WriteJSON(deepgramWSResponse{Type: "Results", IsFinal: true, Channel: &deepgramChannel{Alternatives: []deepgramAlternative{{Transcript: text}}}})
		}
		send("first")
		close(first)
		<-release
		send("second")
		conn.WriteJSON(deepgramWSResponse{Type: "Metadata"})
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	})
	adapter := NewDeepgramAdapter(endpoint, "key", "nova-3", "en", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adapter.Start(ctx, "en"); err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	finished := make(chan error, 1)
	go func() { finished <- adapter.Finalize(ctx) }()
	<-first
	select {
	case err := <-finished:
		t.Errorf("finalized after only first segment: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var texts []string
	for result := range adapter.Results() {
		if result.IsFinal {
			texts = append(texts, result.Text)
		}
	}
	if strings.Join(texts, " ") != "first second" {
		t.Fatalf("segments=%v", texts)
	}
}

func TestOpenAIManualCommitAndRejectedCommit(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "rejected"}[reject], func(t *testing.T) {
			_, endpoint := streamingTestServer(t, func(conn *websocket.Conn, _ *http.Request) {
				_, config, _ := conn.ReadMessage()
				var request struct {
					Session map[string]any `json:"session"`
				}
				json.Unmarshal(config, &request)
				if value, present := request.Session["turn_detection"]; !present || value != nil {
					t.Errorf("turn_detection=%v present=%v", value, present)
				}
				conn.ReadMessage() // append
				conn.ReadMessage() // commit
				if reject {
					conn.WriteJSON(map[string]any{"type": "error", "error": map[string]string{"code": "input_audio_buffer_commit_empty", "message": "not enough audio"}})
				} else {
					conn.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "transcript": "complete", "item_id": "final"})
				}
				conn.ReadMessage()
			})
			adapter := NewOpenAIRealtimeAdapter(endpoint, "key", "gpt-4o-realtime-preview", "en", nil)
			tr := NewStreamingTranscriber(adapter, "en")
			frames := make(chan recording.AudioFrame, 1)
			frames <- recording.AudioFrame{Data: make([]byte, 320)}
			close(frames)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := tr.Start(ctx, frames); err != nil {
				t.Fatal(err)
			}
			err := tr.Stop(ctx)
			if reject && !IsFatalTranscriptionError(err) {
				t.Fatalf("rejected commit error=%v", err)
			}
			if !reject {
				if err != nil {
					t.Fatal(err)
				}
				if text, err := tr.GetFinalTranscription(); err != nil || text != "complete" {
					t.Fatalf("text=%q error=%v", text, err)
				}
			}
		})
	}
}

func TestElevenLabsManualSegmentsPreserveFinalTail(t *testing.T) {
	var bytesReceived atomic.Int64
	_, endpoint := streamingTestServer(t, func(conn *websocket.Conn, r *http.Request) {
		if r.URL.Query().Get("commit_strategy") != "manual" {
			t.Error("manual commit not configured")
		}
		segment := 0
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var chunk elevenLabsInputAudioChunk
			if err := json.Unmarshal(message, &chunk); err != nil {
				t.Error(err)
				return
			}
			audio, err := base64.StdEncoding.DecodeString(chunk.AudioBase64)
			if err != nil {
				t.Error(err)
				return
			}
			bytesReceived.Add(int64(len(audio)))
			if chunk.Commit {
				segment++
				text := "prefix"
				if segment == 2 {
					time.Sleep(50 * time.Millisecond)
					text = "tail"
				}
				conn.WriteJSON(elevenLabsWSMessage{MessageType: "committed_transcript", Text: text})
			}
		}
	})
	adapter := NewElevenLabsStreamingAdapter(endpoint, "key", "scribe_v1", "en", nil)
	tr := NewStreamingTranscriber(adapter, "en")
	frames := make(chan recording.AudioFrame, 1)
	frames <- recording.AudioFrame{Data: make([]byte, 20*16000*2+320)}
	close(frames)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := tr.Start(ctx, frames); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	text, err := tr.GetFinalTranscription()
	if err != nil || text != "prefix tail" {
		t.Fatalf("text=%q error=%v", text, err)
	}
	if bytesReceived.Load() != 20*16000*2+320 {
		t.Fatalf("audio bytes=%d", bytesReceived.Load())
	}
}

func TestStreamingCancellationInterruptsBlockedWrite(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	_, endpoint := streamingTestServer(t, func(*websocket.Conn, *http.Request) { <-release })
	adapter := NewDeepgramAdapter(endpoint, "key", "nova-3", "en", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx, "en"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- adapter.SendChunk(make([]byte, 16*1024*1024)) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("socket cancellation did not interrupt write")
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeepgramProcessingErrorRejectsPartialTranscript(t *testing.T) {
	for name, payload := range map[string]string{
		"nested":    `{"type":"Error","error":{"message":"processing failed"}}`,
		"top-level": `{"type":"Error","description":"processing failed","variant":"BadRequest","code":"DATA-0000"}`,
		"malformed": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, endpoint := streamingTestServer(t, func(conn *websocket.Conn, _ *http.Request) {
				conn.ReadMessage() // CloseStream
				conn.WriteJSON(deepgramWSResponse{Type: "Results", IsFinal: true, Channel: &deepgramChannel{Alternatives: []deepgramAlternative{{Transcript: "incomplete prefix"}}}})
				conn.WriteMessage(websocket.TextMessage, []byte(payload))
				conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			})
			tr := NewStreamingTranscriber(NewDeepgramAdapter(endpoint, "key", "nova-3", "en", nil), "en")
			frames := make(chan recording.AudioFrame)
			close(frames)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := tr.Start(ctx, frames); err != nil {
				t.Fatal(err)
			}
			if err := tr.Stop(ctx); !IsFatalTranscriptionError(err) {
				t.Fatalf("Stop error=%v", err)
			}
			if text, err := tr.GetFinalTranscription(); text != "" || !IsFatalTranscriptionError(err) {
				t.Fatalf("partial text=%q error=%v", text, err)
			}
		})
	}
}
