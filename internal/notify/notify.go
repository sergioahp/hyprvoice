package notify

import (
	"fmt"
	"log"
	"os/exec"
)

type Notifier interface {
	Send(mt MessageType)
	Error(msg string) // for dynamic errors (e.g., pipeline errors)
}

// RecordingNotificationID is the Dunst replace ID for persistent recording notifications
const RecordingNotificationID = 9999

// NewNotifier creates a notifier based on type with resolved messages
func NewNotifier(notifType string, messages map[MessageType]Message) Notifier {
	switch notifType {
	case "desktop":
		return NewDesktop(messages)
	case "log":
		return NewLog(messages)
	default:
		return &Nop{}
	}
}

type Desktop struct {
	messages map[MessageType]Message
}

func NewDesktop(messages map[MessageType]Message) *Desktop {
	return &Desktop{messages: messages}
}

func (d *Desktop) Send(mt MessageType) {
	msg, ok := d.messages[mt]
	if !ok {
		return
	}
	if msg.IsError {
		d.Error(msg.Body)
		return
	}
	// Use persistent notifications (Dunst replace ID 9999) for recording state transitions
	// so all state changes replace the same notification rather than spawning new ones.
	// Context sessions use appname "HyprvoiceCtx" so dunst's rule-hyprvoice-ctx applies
	// a blue tint at the same 38% opacity as the regular pink rule.
	switch mt {
	case MsgRecordingStarted:
		d.persistentNotify("Hyprvoice", msg.Title, "🎤 Recording...", 0)
	case MsgTranscribing:
		d.persistentNotify("Hyprvoice", msg.Title, "⏳ Transcribing...", 0)
	case MsgInjectionCompleted:
		d.persistentNotify("Hyprvoice", msg.Title, "✅ Done", 5000)
	case MsgContextRecordingStarted:
		d.persistentNotify("HyprvoiceCtx", msg.Title, "🎤 Recording...", 0)
	case MsgContextTranscribing:
		d.persistentNotify("HyprvoiceCtx", msg.Title, "⏳ Transcribing...", 0)
	case MsgContextInjectionCompleted:
		d.persistentNotify("HyprvoiceCtx", msg.Title, "✅ Done", 5000)
	case MsgOperationCancelled, MsgRecordingAborted, MsgInjectionAborted:
		d.persistentNotify("Hyprvoice", msg.Title, "❌ Cancelled", 5000)
	default:
		d.notify(msg.Title, msg.Body)
	}
}

func (d *Desktop) Error(msg string) {
	cmd := exec.Command("notify-send", "-a", "Hyprvoice", "-u", "critical", "Hyprvoice Error", msg)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send error notification: %v", err)
	}
}

func (d *Desktop) notify(title, body string) {
	cmd := exec.Command("notify-send", "-a", "Hyprvoice", title, body)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send notification: %v", err)
	}
}

// persistentNotify sends a notification replacing the recording notification via Dunst replace ID.
// appname selects the dunst rule (e.g. "Hyprvoice" for pink, "HyprvoiceCtx" for blue).
// timeoutMs=0 means persistent (no auto-dismiss); >0 auto-dismisses after that many milliseconds.
func (d *Desktop) persistentNotify(appname, title, body string, timeoutMs int) {
	args := []string{
		"-a", appname,
		"-r", fmt.Sprintf("%d", RecordingNotificationID),
		"-t", fmt.Sprintf("%d", timeoutMs),
		title, body,
	}
	cmd := exec.Command("notify-send", args...)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send notification: %v", err)
	}
}

type Log struct {
	messages map[MessageType]Message
}

func NewLog(messages map[MessageType]Message) *Log {
	return &Log{messages: messages}
}

func (l *Log) Send(mt MessageType) {
	msg, ok := l.messages[mt]
	if !ok {
		return
	}
	if msg.IsError {
		l.Error(msg.Body)
		return
	}
	log.Printf("%s: %s", msg.Title, msg.Body)
}

func (l *Log) Error(msg string) {
	log.Printf("Hyprvoice Error: %s", msg)
}

type Nop struct{}

func (Nop) Send(mt MessageType) {}
func (Nop) Error(msg string)    {}
