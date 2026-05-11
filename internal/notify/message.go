package notify

// MessageType identifies a notification event
type MessageType int

const (
	MsgRecordingStarted MessageType = iota
	MsgTranscribing
	MsgLLMProcessing
	MsgConfigReloaded
	MsgOperationCancelled
	MsgRecordingAborted
	MsgInjectionAborted
	// Context variants: same lifecycle as the above but displayed in blue
	// to signal that terminal scrollback is being used as transcription context.
	MsgContextRecordingStarted
	MsgContextTranscribing
	MsgInjectionCompleted
	MsgContextInjectionCompleted
)

// MessageDef defines a message type with its config key and defaults
type MessageDef struct {
	Type         MessageType
	ConfigKey    string // TOML key under [notifications.messages]
	DefaultTitle string
	DefaultBody  string
	IsError      bool // error notifications use critical urgency, no custom title
}

// MessageDefs is the single source of truth for all notification messages
var MessageDefs = []MessageDef{
	{MsgRecordingStarted, "recording_started", "Hyprvoice", "Recording Started", false},
	{MsgTranscribing, "transcribing", "Hyprvoice", "Recording Ended... Transcribing", false},
	{MsgLLMProcessing, "llm_processing", "Hyprvoice", "Processing...", false},
	{MsgConfigReloaded, "config_reloaded", "Hyprvoice", "Config Reloaded", false},
	{MsgOperationCancelled, "operation_cancelled", "Hyprvoice", "Operation Cancelled", false},
	{MsgRecordingAborted, "recording_aborted", "", "Recording Aborted", true},
	{MsgInjectionAborted, "injection_aborted", "", "Injection Aborted", true},
	// context variants share the same config strings; colour is applied in notify.go
	{MsgContextRecordingStarted, "recording_started", "Hyprvoice", "Recording Started", false},
	{MsgContextTranscribing, "transcribing", "Hyprvoice", "Recording Ended... Transcribing", false},
	{MsgInjectionCompleted, "injection_completed", "Hyprvoice", "Done", false},
	{MsgContextInjectionCompleted, "injection_completed", "Hyprvoice", "Done", false},
}

// Message is a resolved message ready for display
type Message struct {
	Title   string
	Body    string
	IsError bool
}
