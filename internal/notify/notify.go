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
	switch mt {
	case MsgRecordingStarted:
		d.persistentNotify(msg.Title, "🎤 Recording...", 0)
	case MsgTranscribing:
		d.persistentNotify(msg.Title, "⏳ Transcribing...", 0)
	case MsgOperationCancelled, MsgRecordingAborted, MsgInjectionAborted:
		d.persistentNotify(msg.Title, "❌ Cancelled", 5000)
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
// timeoutMs=0 means persistent (no auto-dismiss); >0 auto-dismisses after that many milliseconds.
func (d *Desktop) persistentNotify(title, body string, timeoutMs int) {
	cmd := exec.Command("notify-send",
		"-a", "Hyprvoice",
		"-r", fmt.Sprintf("%d", RecordingNotificationID),
		"-t", fmt.Sprintf("%d", timeoutMs),
		title,
		body,
	)
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
