package recording

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRecorderSubprocess(t *testing.T) {
	mode := os.Getenv("HYPRVOICE_TEST_CAPTURE")
	if mode == "" {
		return
	}
	if !slices.Contains(os.Args, "--raw") {
		os.Exit(2)
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	os.WriteFile(os.Getenv("HYPRVOICE_TEST_SPAWNED"), nil, 0600)
	if mode == "eof" {
		os.Exit(0)
	}
	if mode == "descendant" {
		child := exec.Command("sleep", "30")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("HYPRVOICE_TEST_SPAWNED")+".child", []byte(strconv.Itoa(child.Process.Pid)), 0600)
	}
	if mode == "gated" {
		for {
			if _, err := os.Stat(os.Getenv("HYPRVOICE_TEST_GATE")); err == nil {
				break
			}
			select {
			case <-interrupt:
				os.Exit(0)
			case <-time.After(time.Millisecond):
			}
		}
	}
	os.Stdout.Write([]byte{1, 2, 3, 4})
	if mode == "overflow" {
		os.Stdout.Write(make([]byte, 1024*1024))
	}
	<-interrupt
	os.Stdout.Write([]byte{5, 6, 7, 8})
	os.Exit(0)
}

func mockRecorderCommands(t *testing.T, mode string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quotedBinary := "'" + strings.ReplaceAll(binary, "'", "'\\''") + "'"
	commands := map[string]string{
		"pw-cli":    "exit 0\n",
		"pw-record": "exec " + quotedBinary + " -test.run=^TestRecorderSubprocess$ -- \"$@\"\n",
	}
	for name, script := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+shell+"\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	gate, spawned := filepath.Join(dir, "gate"), filepath.Join(dir, "spawned")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HYPRVOICE_TEST_CAPTURE", mode)
	t.Setenv("HYPRVOICE_TEST_GATE", gate)
	t.Setenv("HYPRVOICE_TEST_SPAWNED", spawned)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return gate, spawned
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subprocess did not create %s", path)
}

func testRecorder() *recorder {
	return NewRecorder(Config{SampleRate: 16000, Channels: 1, Format: "s16", BufferSize: 8192, ChannelBufferSize: 30}).(*recorder)
}

func TestRecorder_ReadinessPreservesFirstAndFlushedAudio(t *testing.T) {
	gate, spawned := mockRecorderCommands(t, "gated")
	r := testRecorder()
	r.config.BufferSize = 3
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer r.Stop()
	type result struct {
		frames <-chan AudioFrame
		err    error
	}
	started := make(chan result, 1)
	go func() { frames, _, err := r.Start(ctx); started <- result{frames, err} }()
	waitForFile(t, spawned)
	select {
	case got := <-started:
		t.Fatalf("Start returned before audio: %v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	got := <-started
	if got.err != nil {
		t.Fatal(got.err)
	}
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	r.Stop()
	var audio []byte
	for frame := range got.frames {
		if len(frame.Data)%2 != 0 {
			t.Fatalf("split PCM sample: %v", frame.Data)
		}
		audio = append(audio, frame.Data...)
	}
	if !bytes.Equal(audio, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("audio = %v", audio)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("child was not gracefully reaped: %v", cmd.ProcessState)
	}
	if r.IsRecording() {
		t.Fatal("still recording after Stop")
	}
}

func TestRecorder_CancelDuringStartup(t *testing.T) {
	_, spawned := mockRecorderCommands(t, "gated")
	r := testRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, _, err := r.Start(ctx); finished <- err }()
	waitForFile(t, spawned)
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("startup cancellation succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup cancellation hung")
	}
	r.Stop()
}

func TestRecorder_StartupEOF(t *testing.T) {
	mockRecorderCommands(t, "eof")
	r := testRecorder()
	_, _, err := r.Start(context.Background())
	if err == nil {
		t.Fatal("empty recorder reported ready")
	}
	r.Stop()
}

func TestRecorder_OverflowIsAnError(t *testing.T) {
	mockRecorderCommands(t, "overflow")
	r := testRecorder()
	r.config.ChannelBufferSize = 1
	_, errors, err := r.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	select {
	case err := <-errors:
		if err == nil || !strings.Contains(err.Error(), "audio buffer full") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("overflow was not reported")
	}
}

func TestRecorder_RejectsMislabeledAudio(t *testing.T) {
	for _, cfg := range []Config{
		{SampleRate: 48000, Channels: 1, Format: "s16"},
		{SampleRate: 16000, Channels: 2, Format: "s16"},
		{SampleRate: 16000, Channels: 1, Format: "f32"},
	} {
		t.Run(fmt.Sprint(cfg), func(t *testing.T) {
			if ValidateAudioFormat(cfg.SampleRate, cfg.Channels, cfg.Format) == nil {
				t.Fatal("unsupported PCM accepted")
			}
		})
	}
}

func TestRecorder_StopBoundsInheritedPipe(t *testing.T) {
	_, spawned := mockRecorderCommands(t, "descendant")
	r := testRecorder()
	if _, _, err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	b, err := os.ReadFile(spawned + ".child")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	finished := make(chan struct{})
	go func() { r.Stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("inherited stdout prevented Stop")
	}
}
