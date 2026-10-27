package transcriber

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/recording"
)

func TestStreamingStopDrainsAudioBeforeFinalize(t *testing.T) {
	adapter := NewMockStreamingAdapter()
	entered, release := make(chan struct{}), make(chan struct{})
	var received []byte
	adapter.SendChunkFunc = func(audio []byte) error {
		if len(received) == 0 {
			close(entered)
			<-release
		}
		received = append(received, audio...)
		return nil
	}
	finalized := make(chan struct{}, 1)
	adapter.FinalizeFunc = func(context.Context) error {
		if !bytes.Equal(received, []byte{1, 2, 3, 4}) {
			t.Errorf("audio at finalize = %v", received)
		}
		finalized <- struct{}{}
		adapter.SendResult(TranscriptionResult{Text: "complete", IsFinal: true})
		return nil
	}
	frames := make(chan recording.AudioFrame, 2)
	frames <- recording.AudioFrame{Data: []byte{1, 2}}
	frames <- recording.AudioFrame{Data: []byte{3, 4}}
	close(frames)
	tr := NewStreamingTranscriber(adapter, "en")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := tr.Start(ctx, frames); err != nil {
		t.Fatal(err)
	}
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- tr.Stop(ctx) }()
	select {
	case <-finalized:
		t.Error("finalized while audio send was blocked")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := tr.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 {
		t.Fatal("Finalize was not called exactly once")
	}
	if text, err := tr.GetFinalTranscription(); err != nil || text != "complete" {
		t.Fatalf("transcript=%q error=%v", text, err)
	}
}

func TestStreamingStopReturnsFinalizeError(t *testing.T) {
	adapter := NewMockStreamingAdapter()
	want := errors.New("commit failed")
	adapter.FinalizeFunc = func(context.Context) error { return want }
	frames := make(chan recording.AudioFrame)
	close(frames)
	tr := NewStreamingTranscriber(adapter, "en")
	if _, err := tr.Start(context.Background(), frames); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := tr.Stop(context.Background()); !errors.Is(err, want) {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestStreamingSendFailureRejectsPartialTranscript(t *testing.T) {
	adapter := NewMockStreamingAdapter()
	adapter.SendChunkFunc = func([]byte) error { return errors.New("send failed") }
	frames := make(chan recording.AudioFrame, 1)
	frames <- recording.AudioFrame{Data: []byte{1, 2}}
	close(frames)
	tr := NewStreamingTranscriber(adapter, "en")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errors, err := tr.Start(ctx, frames)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errors:
		if !IsFatalTranscriptionError(err) {
			t.Fatalf("error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := tr.Stop(ctx); !IsFatalTranscriptionError(err) {
		t.Fatalf("Stop error=%v", err)
	}
	if _, err := tr.GetFinalTranscription(); !IsFatalTranscriptionError(err) {
		t.Fatalf("transcript error=%v", err)
	}
}

func TestStreamingStopPreservesLateResultsAndErrors(t *testing.T) {
	adapter := NewMockStreamingAdapter()
	adapter.CloseFunc = func() error {
		time.Sleep(150 * time.Millisecond)
		adapter.SendResult(TranscriptionResult{Error: NewFatalTranscriptionError(errors.New("late fatal error"))})
		close(adapter.resultsCh)
		return nil
	}
	frames := make(chan recording.AudioFrame)
	close(frames)
	tr := NewStreamingTranscriber(adapter, "en")
	if _, err := tr.Start(context.Background(), frames); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stop(context.Background()); !IsFatalTranscriptionError(err) {
		t.Fatalf("Stop error=%v", err)
	}
}

func TestSimpleStopReturnsOriginalErrorWithoutRetry(t *testing.T) {
	calls := 0
	want := errors.New("transcription failed")
	adapter := &MockBatchAdapter{TranscribeFunc: func(context.Context, []byte) (string, error) { calls++; return "", want }}
	tr := NewSimpleTranscriber(Config{}, adapter)
	frames := make(chan recording.AudioFrame, 1)
	frames <- recording.AudioFrame{Data: []byte{1, 2}}
	close(frames)
	if _, err := tr.Start(context.Background(), frames); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := tr.Stop(context.Background()); !errors.Is(err, want) {
			t.Fatalf("error=%v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("requests=%d", calls)
	}
}
