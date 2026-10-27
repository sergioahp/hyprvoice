package transcriber

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/leonardotrapani/hyprvoice/internal/provider"
)

// ElevenLabsStreamingAdapter implements StreamingAdapter for ElevenLabs real-time transcription
type ElevenLabsStreamingAdapter struct {
	endpoint     *provider.EndpointConfig
	apiKey       string
	model        string
	language     string
	keywords     []string
	conn         *websocket.Conn
	resultsCh    chan TranscriptionResult
	mu           sync.Mutex
	sendMu       sync.Mutex
	segmentBytes int
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	started      bool

	// finalization signaling
	commitDone  chan struct{}
	contextSent bool
}

// ElevenLabs WebSocket message types (outgoing)
type elevenLabsInputAudioChunk struct {
	MessageType  string `json:"message_type"`
	AudioBase64  string `json:"audio_base_64"`
	Commit       bool   `json:"commit"`
	SampleRate   int    `json:"sample_rate"`
	PreviousText string `json:"previous_text,omitempty"`
}

// ElevenLabs WebSocket response types (incoming)
type elevenLabsWSMessage struct {
	MessageType  string `json:"message_type"`
	Text         string `json:"text,omitempty"`
	Error        string `json:"error,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// NewElevenLabsStreamingAdapter creates a new streaming adapter for ElevenLabs
// endpoint: the WebSocket endpoint config (e.g., wss://api.elevenlabs.io, /v1/speech-to-text/realtime)
// apiKey: ElevenLabs API key
// model: model ID (e.g., "scribe_v1")
// lang: provider language code
func NewElevenLabsStreamingAdapter(endpoint *provider.EndpointConfig, apiKey, model, lang string, keywords []string) *ElevenLabsStreamingAdapter {
	return &ElevenLabsStreamingAdapter{
		endpoint:   endpoint,
		apiKey:     apiKey,
		model:      model,
		language:   lang,
		keywords:   keywords,
		resultsCh:  make(chan TranscriptionResult, 100),
		commitDone: make(chan struct{}, 1),
	}
}

// Start initiates the WebSocket connection to ElevenLabs
func (a *ElevenLabsStreamingAdapter) Start(ctx context.Context, lang string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started {
		return fmt.Errorf("adapter already started")
	}

	// use lang param if provided, otherwise use constructor lang
	if lang != "" {
		a.language = lang
	}

	// create cancelable context
	a.ctx, a.cancel = context.WithCancel(ctx)

	// connect to WebSocket
	if err := a.connectLocked(); err != nil {
		a.cancel()
		return err
	}
	a.started = true

	// start reader goroutine
	a.wg.Add(1)
	go a.readLoop()

	log.Printf("elevenlabs-streaming: connected, model=%s, language=%s", a.model, a.language)
	return nil
}

// connectLocked establishes WebSocket connection. Must be called with mu held.
func (a *ElevenLabsStreamingAdapter) connectLocked() error {
	wsURL, err := a.buildURL()
	if err != nil {
		return fmt.Errorf("build websocket url: %w", err)
	}

	headers := http.Header{}
	headers.Set("xi-api-key", a.apiKey)

	log.Printf("elevenlabs-streaming: connecting to %s", wsURL)
	conn, resp, err := websocket.DefaultDialer.DialContext(a.ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			log.Printf("elevenlabs-streaming: dial failed with status %d", resp.StatusCode)
		}
		return fmt.Errorf("websocket dial: %w", err)
	}
	a.conn = conn
	bindStreamingSocket(a.ctx, conn)
	a.contextSent = false
	return nil
}

// buildURL constructs the WebSocket URL with query parameters
func (a *ElevenLabsStreamingAdapter) buildURL() (string, error) {
	// parse base URL and path
	baseURL := a.endpoint.BaseURL + a.endpoint.Path

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}

	// add query parameters
	q := u.Query()
	q.Set("model_id", a.model)
	q.Set("audio_format", "pcm_16000") // we use 16kHz PCM

	// add language if specified
	if a.language != "" {
		q.Set("language_code", a.language)
	}

	// Explicit commits let Finalize identify completion of the final segment.
	q.Set("commit_strategy", "manual")

	u.RawQuery = q.Encode()
	return u.String(), nil
}

// readLoop reads messages from the WebSocket and sends results to the channel
func (a *ElevenLabsStreamingAdapter) readLoop() {
	defer a.wg.Done()
	defer close(a.resultsCh)

	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		a.mu.Lock()
		conn := a.conn
		a.mu.Unlock()

		if conn == nil {
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: NewFatalTranscriptionError(fmt.Errorf("streaming connection is closed"))})
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if a.handleFatalClose(err) {
				return
			}
			// check if context was cancelled (normal shutdown)
			select {
			case <-a.ctx.Done():
				return
			default:
			}

			fatal := NewFatalTranscriptionError(fmt.Errorf("websocket read: %w; recording cannot be resumed without audio loss", err))
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: fatal})
			a.cancel()
			return
		}

		// parse message
		var msg elevenLabsWSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: NewFatalTranscriptionError(fmt.Errorf("elevenlabs-streaming: invalid server response: %w", err))})
			a.cancel()
			return
		}

		// handle different message types
		switch msg.MessageType {
		case "session_started":
			log.Printf("elevenlabs-streaming: session started, id=%s", msg.SessionID)

		case "partial_transcript":
			// interim result
			if msg.Text != "" {
				emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Text: msg.Text, IsFinal: false})
			}

		case "committed_transcript", "committed_transcript_with_timestamps":
			// final result
			log.Printf("elevenlabs-streaming: committed: %q", msg.Text)
			if msg.Text != "" {
				emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Text: msg.Text, IsFinal: true})
			}
			// signal finalization is done (non-blocking)
			select {
			case a.commitDone <- struct{}{}:
			default:
			}

		case "error", "auth_error", "quota_exceeded", "rate_limited",
			"queue_overflow", "resource_exhausted", "session_time_limit_exceeded",
			"input_error", "chunk_size_exceeded", "insufficient_audio_activity",
			"transcriber_error", "commit_throttled", "unaccepted_terms", "invalid_request":
			// error message
			errMsg := msg.Error
			if errMsg == "" {
				errMsg = msg.MessageType
			}
			log.Printf("elevenlabs-streaming: error: %s", errMsg)
			err := fmt.Errorf("elevenlabs: %s", errMsg)
			a.handleFatalError(err)
			return

		default:
			log.Printf("elevenlabs-streaming: unknown message type: %s payload=%s", msg.MessageType, strings.TrimSpace(string(message)))
		}
	}
}

func (a *ElevenLabsStreamingAdapter) emitResultError(err error) {
	emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: err})
}

func (a *ElevenLabsStreamingAdapter) handleFatalError(err error) {
	fatalErr := NewFatalTranscriptionError(err)
	log.Printf("elevenlabs-streaming: fatal error: %v", err)
	a.emitResultError(fatalErr)
	a.closeConn()
	if a.cancel != nil {
		a.cancel()
	}
}

func (a *ElevenLabsStreamingAdapter) handleFatalClose(err error) bool {
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return false
	}
	if !isElevenLabsFatalCloseCode(closeErr.Code) {
		return false
	}
	reason := strings.TrimSpace(closeErr.Text)
	if reason == "" {
		reason = "no reason provided"
	}
	a.handleFatalError(fmt.Errorf("elevenlabs websocket closed (%d): %s", closeErr.Code, reason))
	return true
}

func (a *ElevenLabsStreamingAdapter) closeConn() {
	a.mu.Lock()
	conn := a.conn
	a.conn = nil
	a.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func isElevenLabsFatalCloseCode(code int) bool {
	switch code {
	case websocket.ClosePolicyViolation,
		websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData,
		websocket.CloseMessageTooBig,
		websocket.CloseProtocolError:
		return true
	default:
		return false
	}
}

// SendChunk sends audio data to the WebSocket
func (a *ElevenLabsStreamingAdapter) SendChunk(audio []byte) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	for len(audio) > 0 {
		end := min(len(audio), 32000)
		if err := a.sendChunk(audio[:end]); err != nil {
			return err
		}
		audio = audio[end:]
	}
	return nil
}

func (a *ElevenLabsStreamingAdapter) sendChunk(audio []byte) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return fmt.Errorf("adapter not started")
	}
	conn := a.conn
	a.mu.Unlock()

	// check context
	select {
	case <-a.ctx.Done():
		return a.ctx.Err()
	default:
	}

	if conn == nil {
		return fmt.Errorf("no connection")
	}

	// encode audio as base64
	audioB64 := base64.StdEncoding.EncodeToString(audio)

	// create message
	msg := elevenLabsInputAudioChunk{
		MessageType: "input_audio_chunk",
		AudioBase64: audioB64,
		Commit:      a.segmentBytes+len(audio) >= 20*16000*2,
		SampleRate:  16000,
	}

	a.mu.Lock()
	if !a.contextSent && len(a.keywords) > 0 {
		msg.PreviousText = strings.Join(a.keywords, ", ")
		a.contextSent = true
	}
	a.mu.Unlock()

	if msg.Commit {
		select {
		case <-a.commitDone:
		default:
		}
	}
	// send as JSON
	a.mu.Lock()
	err := writeStreamingJSON(a.ctx, conn, msg)
	a.mu.Unlock()

	if err != nil {
		return NewFatalTranscriptionError(fmt.Errorf("websocket write: %w", err))
	}
	a.segmentBytes += len(audio)
	// ElevenLabs automatically commits at about 36 seconds even in manual mode.
	// Commit and await each 20-second segment to keep responses unambiguous.
	if msg.Commit {
		if err := a.waitCommit(a.ctx); err != nil {
			return err
		}
		a.segmentBytes = 0
	}

	return nil
}

// Results returns the channel for receiving transcription results
func (a *ElevenLabsStreamingAdapter) Results() <-chan TranscriptionResult {
	return a.resultsCh
}

// Finalize sends a commit message to force ElevenLabs to commit any pending audio
// and waits for the committed_transcript response
func (a *ElevenLabsStreamingAdapter) Finalize(ctx context.Context) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.segmentBytes == 0 {
		return nil
	}
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	conn := a.conn
	a.mu.Unlock()

	if conn == nil {
		return nil
	}

	// drain any previous commit signals
	select {
	case <-a.commitDone:
	default:
	}

	// send empty audio chunk with commit=true to force finalization
	msg := elevenLabsInputAudioChunk{
		MessageType: "input_audio_chunk",
		AudioBase64: "",
		Commit:      true,
		SampleRate:  16000,
	}

	a.mu.Lock()
	err := writeStreamingJSON(a.ctx, conn, msg)
	a.mu.Unlock()

	if err != nil {
		log.Printf("elevenlabs-streaming: finalize write error: %v", err)
		return fmt.Errorf("finalize write: %w", err)
	}

	log.Printf("elevenlabs-streaming: sent commit, waiting for final transcript")

	if err := a.waitCommit(ctx); err != nil {
		return err
	}
	a.segmentBytes = 0
	return nil
}

func (a *ElevenLabsStreamingAdapter) waitCommit(ctx context.Context) error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-a.commitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ctx.Done():
		return a.ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timed out waiting for ElevenLabs commit")
	}
}

// Close gracefully closes the WebSocket connection
func (a *ElevenLabsStreamingAdapter) Close() error {
	a.mu.Lock()

	if !a.started {
		a.mu.Unlock()
		return nil
	}

	// cancel context first to signal reader to stop
	if a.cancel != nil {
		a.cancel()
	}

	// get conn ref while holding lock
	conn := a.conn

	a.started = false
	a.mu.Unlock()

	// close websocket outside of lock (readLoop may be blocked on read)
	if conn != nil {
		conn.Close()
	}

	// wait for reader to finish
	a.wg.Wait()

	log.Printf("elevenlabs-streaming: closed")
	return nil
}
