package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/notify"
	"github.com/leonardotrapani/hyprvoice/internal/testutil"
)

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
	case _, ok := <-p.GetNotifyCh():
		if ok {
			t.Fatal("unexpected notification")
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
