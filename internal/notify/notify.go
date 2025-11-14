package notify

import (
	"fmt"
	"github.com/leonardotrapani/hyprvoice/internal/config"
	"log"
	"os/exec"
)

type Notifier interface {
	Error(msg string)
	Notify(title, message string)
	RecordingStarted()
	Transcribing()
	RecordingComplete()
}

// Dunst replace IDs for persistent notifications
const (
	RecordingNotificationID = 9999 // Persistent notification during recording/transcribing
)

type Desktop struct{}

func (d Desktop) RecordingStarted() {
	d.PersistentNotify("Hyprvoice", "🎤 Recording...", RecordingNotificationID)
}

func (d Desktop) Transcribing() {
	d.PersistentNotify("Hyprvoice", "⏳ Transcribing...", RecordingNotificationID)
}

func (d Desktop) RecordingComplete() {
	// Replace the persistent notification with a timed one (5 seconds)
	cmd := exec.Command("notify-send",
		"-a", "Hyprvoice",
		"-r", fmt.Sprintf("%d", RecordingNotificationID), // Replace ID to update existing notification
		"-t", "5000",                                       // 5 second timeout
		"Hyprvoice",
		"✅ Complete",
	)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send completion notification: %v", err)
	}
}

func (Desktop) Error(msg string) {
	cmd := exec.Command("notify-send", "-a", "Hyprvoice", "-u", "critical", "Hyprvoice Error", msg)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send error notification: %v", err)
	}
}

func (Desktop) Notify(title, message string) {
	cmd := exec.Command("notify-send", "-a", "Hyprvoice", title, message)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send notification: %v", err)
	}
}

// PersistentNotify sends a notification with a replace ID for Dunst
// This allows the notification to persist and be replaced rather than spawning new ones
func (Desktop) PersistentNotify(title, message string, replaceID int) {
	cmd := exec.Command("notify-send",
		"-a", "Hyprvoice",
		"-r", fmt.Sprintf("%d", replaceID), // Replace ID for Dunst
		"-t", "0",                           // Timeout 0 = persistent until replaced
		title,
		message,
	)
	if err := cmd.Run(); err != nil {
		log.Printf("Failed to send persistent notification: %v", err)
	}
}

type Log struct{}

func (l Log) Error(msg string) {
	l.Notify("Hyprvoice Error", msg)
}

func (Log) Notify(title, message string) {
	log.Printf("%s: %s", title, message)
}

func (l Log) RecordingStarted() {
	l.Notify("Hyprvoice", "🎤 Recording...")
}

func (l Log) Transcribing() {
	l.Notify("Hyprvoice", "⏳ Transcribing...")
}

func (l Log) RecordingComplete() {
	l.Notify("Hyprvoice", "✅ Complete")
}

type Nop struct{}

func (Nop) Error(msg string)             {}
func (Nop) Notify(title, message string) {}
func (Nop) RecordingStarted()            {}
func (Nop) Transcribing()                {}
func (Nop) RecordingComplete()           {}

func GetNotifierBasedOnConfig(c *config.Config) Notifier {
	switch c.Notifications.Type {
	case "desktop":
		return Desktop{}
	case "log":
		return Log{}
	case "none":
		return Nop{}
	}
	return Nop{}
}
