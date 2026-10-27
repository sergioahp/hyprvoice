package notify

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
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
	if msg.IsError && mt != MsgRecordingAborted && mt != MsgInjectionAborted {
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
		title := msg.Title
		if title == "" {
			title = "Hyprvoice"
		}
		d.persistentNotify("Hyprvoice", title, "❌ Cancelled", 5000)
	default:
		d.notify(msg.Title, msg.Body)
	}
}

func (d *Desktop) Error(msg string) {
	if err := runNotify("-a", "Hyprvoice", "-u", "critical", "Hyprvoice Error", msg); err != nil {
		log.Printf("Failed to send error notification: %v", err)
	}
}

func (d *Desktop) notify(title, body string) {
	if err := runNotify("-a", "Hyprvoice", title, body); err != nil {
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
	if err := runNotify(args...); err != nil {
		log.Printf("Failed to send notification: %v", err)
	}
}

func runNotify(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "notify-send", args...).Run()
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
