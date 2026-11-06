package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/injection"
	"github.com/leonardotrapani/hyprvoice/internal/notify"
	"github.com/leonardotrapani/hyprvoice/internal/testutil"
	"github.com/leonardotrapani/hyprvoice/internal/transcriber"
)

func lastNotification(t *testing.T, p Pipeline) notify.MessageType {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var last notify.MessageType
	for {
		select {
		case mt, ok := <-p.GetNotifyCh():
			if !ok {
				return last
			}
			last = mt
		case <-deadline.C:
			t.Fatal("pipeline did not finish")
		}
	}
}

func TestPipelineReadinessAndCompletion(t *testing.T) {
	for _, prompt := range []string{"", "terminal context"} {
		t.Run(prompt, func(t *testing.T) {
			p := New(testutil.TestConfig(),
				WithContextPrompt(prompt),
				WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
				WithTranscriberFactory(testutil.MockTranscriberFactory(testutil.NewMockTranscriber("hello"))),
				WithInjectorFactory(testutil.MockInjectorFactory(testutil.NewMockInjector())),
			)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			defer p.Stop()
			p.Run(ctx)
			if status := p.Status(); status == Idle || status == "" {
				t.Fatalf("status after Run=%q", status)
			}
			want := notify.MsgRecordingStarted
			if prompt != "" {
				want = notify.MsgContextRecordingStarted
			}
			select {
			case got := <-p.GetNotifyCh():
				if got != want {
					t.Fatalf("notification=%v want=%v", got, want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			p.GetActionCh() <- Inject
			for range p.GetNotifyCh() {
			}
			p.Stop()
			if _, ok := <-p.GetErrorCh(); ok {
				t.Fatal("error channel not closed")
			}
			if p.Status() != Idle {
				t.Fatal("pipeline not idle")
			}
		})
	}
}

func TestPipelineStartupFailureDoesNotAnnounceRecording(t *testing.T) {
	r := testutil.NewMockRecorder()
	r.StartError = errors.New("microphone failed")
	p := New(testutil.TestConfig(), WithRecorderFactory(testutil.MockRecorderFactory(r)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Run(ctx)
	select {
	case err := <-p.GetErrorCh():
		if !errors.Is(err.Err, r.StartError) {
			t.Fatalf("error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	p.Stop()
	if notification, ok := <-p.GetNotifyCh(); ok {
		t.Fatalf("unexpected notification=%v", notification)
	}
}

func TestPipelineCancelActionCompletes(t *testing.T) {
	p := New(testutil.TestConfig(),
		WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
		WithTranscriberFactory(testutil.MockTranscriberFactory(testutil.NewMockTranscriber("unused"))),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Run(ctx)
	<-p.GetNotifyCh()
	p.GetActionCh() <- Cancel
	select {
	case mt, ok := <-p.GetNotifyCh():
		if !ok || mt != notify.MsgOperationCancelled {
			t.Fatalf("cancel notification=%v, open=%v", mt, ok)
		}
	case <-ctx.Done():
		t.Fatal("cancel action did not stop pipeline")
	}
	p.Stop()
}

func TestPipelineConcurrentRunAndStop(t *testing.T) {
	for range 30 {
		p := New(testutil.TestConfig(),
			WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
			WithTranscriberFactory(testutil.MockTranscriberFactory(testutil.NewMockTranscriber("unused"))),
		)
		started := make(chan struct{})
		go func() { p.Run(context.Background()); close(started) }()
		p.Stop()
		<-started
		p.Stop()
		if p.Status() != Idle {
			t.Fatal("pipeline restarted after Stop")
		}
	}
}

func TestPipelineTimeoutReplacesRecordingNotification(t *testing.T) {
	cfg := testutil.TestConfig()
	cfg.Recording.Timeout = 50 * time.Millisecond
	mockTranscriber := testutil.NewMockTranscriber("unused")
	mockTranscriber.StopError = errors.New("upload failed")
	p := New(cfg,
		WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
		WithTranscriberFactory(testutil.MockTranscriberFactory(mockTranscriber)),
		WithInjectorFactory(testutil.MockInjectorFactory(testutil.NewMockInjector())),
	)
	p.Run(context.Background())
	defer p.Stop()
	if last := lastNotification(t, p); last != notify.MsgOperationCancelled {
		t.Fatalf("terminal notification=%v", last)
	}
}

func TestPipelineTimeoutTranscribesCapturedAudio(t *testing.T) {
	cfg := testutil.TestConfig()
	cfg.Recording.Timeout = 50 * time.Millisecond
	var audioBytes int
	var transcribeCtxErr error
	adapter := &testutil.MockTranscriberAdapter{
		TranscribeFunc: func(ctx context.Context, audio []byte) (string, error) {
			audioBytes, transcribeCtxErr = len(audio), ctx.Err()
			return "hello", ctx.Err()
		},
	}
	injector := testutil.NewMockInjector()
	var backends []string
	p := New(cfg,
		WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
		WithTranscriberFactory(func(c transcriber.Config) (transcriber.Transcriber, error) {
			return transcriber.NewSimpleTranscriber(c, adapter), nil
		}),
		WithInjectorFactory(func(c injection.Config) injection.Injector {
			backends = c.Backends
			return injector
		}),
	)
	p.Run(context.Background())
	defer p.Stop()
	if last := lastNotification(t, p); last != notify.MsgInjectionCompleted {
		t.Fatalf("terminal notification=%v", last)
	}
	if audioBytes == 0 || transcribeCtxErr != nil {
		t.Fatalf("audioBytes=%d ctxErr=%v", audioBytes, transcribeCtxErr)
	}
	if got := injector.GetInjectedTexts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("injected=%q", got)
	}
	if len(backends) != 1 || backends[0] != "clipboard" {
		t.Fatalf("unattended injection backends=%q", backends)
	}
}

func TestRecordingTimeoutDoesNotBoundUpload(t *testing.T) {
	cfg := testutil.TestConfig()
	cfg.Recording.Timeout = 50 * time.Millisecond
	adapter := &testutil.MockTranscriberAdapter{
		TranscribeFunc: func(ctx context.Context, audio []byte) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(150 * time.Millisecond):
				return "hello", nil
			}
		},
	}
	p := New(cfg,
		WithRecorderFactory(testutil.MockRecorderFactory(testutil.NewMockRecorder())),
		WithTranscriberFactory(func(c transcriber.Config) (transcriber.Transcriber, error) {
			return transcriber.NewSimpleTranscriber(c, adapter), nil
		}),
		WithInjectorFactory(testutil.MockInjectorFactory(testutil.NewMockInjector())),
	)
	p.Run(context.Background())
	defer p.Stop()
	p.GetActionCh() <- Inject
	if last := lastNotification(t, p); last != notify.MsgInjectionCompleted {
		t.Fatalf("terminal notification=%v", last)
	}
}
