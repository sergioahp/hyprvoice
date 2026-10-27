package notify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMessages() map[MessageType]Message {
	return map[MessageType]Message{
		MsgRecordingStarted:   {Title: "Hyprvoice", Body: "Recording Started", IsError: false},
		MsgTranscribing:       {Title: "Hyprvoice", Body: "Transcribing", IsError: false},
		MsgConfigReloaded:     {Title: "Hyprvoice", Body: "Config Reloaded", IsError: false},
		MsgOperationCancelled: {Title: "Hyprvoice", Body: "Operation Cancelled", IsError: false},
		MsgRecordingAborted:   {Title: "", Body: "Recording Aborted", IsError: true},
		MsgInjectionAborted:   {Title: "", Body: "Injection Aborted", IsError: true},
	}
}

func TestDesktop_Send(t *testing.T) {
	mockNotifyCommand(t, "exit 0\n")
	desktop := NewDesktop(testMessages())

	// The command helper isolates these notifications from the desktop.
	desktop.Send(MsgRecordingStarted)
	desktop.Send(MsgTranscribing)
	desktop.Send(MsgRecordingAborted) // error type
}

func TestDesktop_Error(t *testing.T) {
	mockNotifyCommand(t, "exit 0\n")
	desktop := NewDesktop(testMessages())
	desktop.Error("Test Error Message")
}

func TestLog_Send(t *testing.T) {
	logNotifier := NewLog(testMessages())

	logNotifier.Send(MsgRecordingStarted)
	logNotifier.Send(MsgRecordingAborted) // error type
}

func TestLog_Error(t *testing.T) {
	logNotifier := NewLog(testMessages())
	logNotifier.Error("Test Error Message")
}

func TestNop_Send(t *testing.T) {
	nop := Nop{}
	nop.Send(MsgRecordingStarted)
	nop.Send(MsgRecordingAborted)
}

func TestNop_Error(t *testing.T) {
	nop := Nop{}
	nop.Error("Test Error Message")
}

func TestNewNotifier(t *testing.T) {
	msgs := testMessages()

	tests := []struct {
		name       string
		notifType  string
		expectType string
	}{
		{"desktop", "desktop", "*notify.Desktop"},
		{"log", "log", "*notify.Log"},
		{"none", "none", "*notify.Nop"},
		{"unknown", "unknown", "*notify.Nop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier := NewNotifier(tt.notifType, msgs)

			// Test the notifier works
			notifier.Send(MsgRecordingStarted)
			notifier.Error("Error")
		})
	}
}

func TestNotifierInterface(t *testing.T) {
	msgs := testMessages()

	// Test that all notifiers implement the Notifier interface
	var notifier Notifier

	notifier = NewDesktop(msgs)
	notifier.Send(MsgRecordingStarted)
	notifier.Error("Error")

	notifier = NewLog(msgs)
	notifier.Send(MsgRecordingStarted)
	notifier.Error("Error")

	notifier = &Nop{}
	notifier.Send(MsgRecordingStarted)
	notifier.Error("Error")
}

func TestMessageDefs(t *testing.T) {
	// Verify MessageDefs contains expected entries
	if len(MessageDefs) != 11 {
		t.Errorf("Expected 11 MessageDefs, got %d", len(MessageDefs))
	}

	// Verify each has required fields
	for _, def := range MessageDefs {
		if def.ConfigKey == "" {
			t.Errorf("MessageDef type %d has empty ConfigKey", def.Type)
		}
		if def.DefaultBody == "" {
			t.Errorf("MessageDef type %d has empty DefaultBody", def.Type)
		}
	}
}

func TestSend_UnknownMessageType(t *testing.T) {
	msgs := testMessages()
	desktop := NewDesktop(msgs)

	// Should not panic with unknown message type
	desktop.Send(MessageType(999))
}

func mockNotifyCommand(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notify-send")
	if err := os.WriteFile(path, []byte("#!"+shell+"\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestDesktop_AbortReplacesRecording(t *testing.T) {
	dir := mockNotifyCommand(t, "printf '%s\\n' \"$@\" > \"$HYPRVOICE_TEST_NOTIFY_ARGS\"\n")
	path := filepath.Join(dir, "args")
	t.Setenv("HYPRVOICE_TEST_NOTIFY_ARGS", path)
	desktop := NewDesktop(testMessages())
	desktop.Send(MsgRecordingAborted)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "-r\n9999\n") || !strings.Contains(string(b), "Hyprvoice\n") {
		t.Fatalf("arguments=%q", b)
	}
}

func TestDesktop_NotificationTimeout(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	mockNotifyCommand(t, "exec '"+strings.ReplaceAll(sleep, "'", "'\\''")+"' 30\n")
	finished := make(chan struct{})
	go func() { NewDesktop(testMessages()).Send(MsgRecordingStarted); close(finished) }()
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("notification command did not time out")
	}
}
