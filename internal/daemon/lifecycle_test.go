package daemon

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/notify"
	"github.com/leonardotrapani/hyprvoice/internal/pipeline"
)

func newLifecycleDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "hyprvoice", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testConfigContent), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.cancel(); d.stopPipeline() })
	return d
}

type lifecyclePipeline struct {
	MockPipeline
	state         pipeline.Status
	actions       chan pipeline.Action
	stopped       bool
	errors        chan pipeline.PipelineError
	notifications chan notify.MessageType
}

func (p *lifecyclePipeline) Status() pipeline.Status                   { return p.state }
func (p *lifecyclePipeline) Stop()                                     { p.stopped = true }
func (p *lifecyclePipeline) GetActionCh() chan<- pipeline.Action       { return p.actions }
func (p *lifecyclePipeline) GetErrorCh() <-chan pipeline.PipelineError { return p.errors }
func (p *lifecyclePipeline) GetNotifyCh() <-chan notify.MessageType    { return p.notifications }

func TestToggleCancelsLLMProcessing(t *testing.T) {
	d := newLifecycleDaemon(t)
	p := &lifecyclePipeline{state: pipeline.Processing}
	d.pipeline = p
	d.toggle("")
	if !p.stopped || d.status() != pipeline.Idle {
		t.Fatal("processing was not cancelled")
	}
}

func TestDuplicateInjectDoesNotBlock(t *testing.T) {
	d := newLifecycleDaemon(t)
	p := &lifecyclePipeline{state: pipeline.Transcribing, actions: make(chan pipeline.Action, 1)}
	p.actions <- pipeline.Inject
	d.pipeline = p
	done := make(chan struct{})
	go func() { d.toggle(""); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("duplicate inject blocked")
	}
}

func TestMonitorsExitOnClosedChannels(t *testing.T) {
	d := newLifecycleDaemon(t)
	p := &lifecyclePipeline{errors: make(chan pipeline.PipelineError), notifications: make(chan notify.MessageType)}
	close(p.errors)
	close(p.notifications)
	done := make(chan struct{})
	go func() { d.monitorPipelineErrors(p); d.monitorPipelineNotifications(p); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitors did not exit")
	}
}

func TestReadBodyRejectsOversizedContext(t *testing.T) {
	d := newLifecycleDaemon(t)
	for _, header := range []string{"t 1048577\n", "t -1\n", "t invalid\n"} {
		if _, err := d.readBody(bufio.NewReader(strings.NewReader("")), header); err == nil {
			t.Fatalf("accepted %q", header)
		}
	}
}
