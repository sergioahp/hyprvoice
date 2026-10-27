package transcriber

import (
	"context"
	"encoding/json"
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

// DeepgramAdapter implements StreamingAdapter for Deepgram real-time transcription
type DeepgramAdapter struct {
	endpoint  *provider.EndpointConfig
	apiKey    string
	model     string
	language  string
	keywords  []string
	conn      *websocket.Conn
	resultsCh chan TranscriptionResult
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	started   bool

	// finalization signaling
	finalizeDone chan error
	finalizing   bool // true when Finalize() has been called
}

// deepgramCloseStream message to signal end of audio
type deepgramCloseStream struct {
	Type string `json:"type"`
}

// Deepgram WebSocket response types (incoming)
type deepgramWSResponse struct {
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Variant     string            `json:"variant,omitempty"`
	Code        string            `json:"code,omitempty"`
	Channel     *deepgramChannel  `json:"channel,omitempty"`
	Metadata    *deepgramMetadata `json:"metadata,omitempty"`
	Error       *deepgramError    `json:"error,omitempty"`
	ChannelIdx  []int             `json:"channel_index,omitempty"`
	Duration    float64           `json:"duration,omitempty"`
	Start       float64           `json:"start,omitempty"`
	IsFinal     bool              `json:"is_final,omitempty"`
	SpeechFinal bool              `json:"speech_final,omitempty"`
}

type deepgramChannel struct {
	Alternatives []deepgramAlternative `json:"alternatives,omitempty"`
}

type deepgramAlternative struct {
	Transcript string  `json:"transcript"`
	Confidence float64 `json:"confidence"`
}

type deepgramMetadata struct {
	RequestID string `json:"request_id"`
	ModelInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"model_info"`
}

type deepgramError struct {
	Type        string `json:"type"`
	Message     string `json:"message"`
	Description string `json:"description,omitempty"`
}

// NewDeepgramAdapter creates a new streaming adapter for Deepgram
// endpoint: the WebSocket endpoint config (e.g., wss://api.deepgram.com, /v1/listen)
// apiKey: Deepgram API key
// model: model ID (e.g., "nova-3")
// lang: provider language code
func NewDeepgramAdapter(endpoint *provider.EndpointConfig, apiKey, model, lang string, keywords []string) *DeepgramAdapter {
	return &DeepgramAdapter{
		endpoint:     endpoint,
		apiKey:       apiKey,
		model:        model,
		language:     lang,
		keywords:     keywords,
		resultsCh:    make(chan TranscriptionResult, 100),
		finalizeDone: make(chan error, 1),
	}
}

// Start initiates the WebSocket connection to Deepgram
func (a *DeepgramAdapter) Start(ctx context.Context, lang string) error {
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

	log.Printf("deepgram: connected, model=%s, language=%s", a.model, a.language)
	return nil
}

// connectLocked establishes WebSocket connection. Must be called with mu held.
func (a *DeepgramAdapter) connectLocked() error {
	wsURL, err := a.buildURL()
	if err != nil {
		return fmt.Errorf("build websocket url: %w", err)
	}

	headers := http.Header{}
	headers.Set("Authorization", "Token "+a.apiKey)

	log.Printf("deepgram: connecting to %s", wsURL)
	conn, resp, err := websocket.DefaultDialer.DialContext(a.ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			log.Printf("deepgram: dial failed with status %d", resp.StatusCode)
		}
		return fmt.Errorf("websocket dial: %w", err)
	}
	a.conn = conn
	bindStreamingSocket(a.ctx, conn)
	return nil
}

// buildURL constructs the WebSocket URL with query parameters
func (a *DeepgramAdapter) buildURL() (string, error) {
	// parse base URL and path
	baseURL := a.endpoint.BaseURL + a.endpoint.Path

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}

	// add query parameters
	q := u.Query()
	q.Set("model", a.model)
	q.Set("encoding", "linear16") // 16-bit linear PCM
	q.Set("sample_rate", "16000") // 16kHz
	q.Set("channels", "1")        // mono

	// enable interim results
	q.Set("interim_results", "true")
	// enable smart formatting for better output
	q.Set("smart_format", "true")
	// enable punctuation
	q.Set("punctuate", "true")

	// add language if specified
	lang := normalizeDeepgramLanguage(a.language)
	if lang != "" {
		q.Set("language", lang)
	}

	// nova-3 uses "keyterm" (singular), others use "keywords" (plural)
	if len(a.keywords) > 0 && !strings.HasPrefix(a.model, "nova-3") && !strings.HasPrefix(a.model, "flux") {
		q.Set("keywords", strings.Join(a.keywords, ","))
	}

	u.RawQuery = q.Encode()
	return u.String(), nil
}

// readLoop reads messages from the WebSocket and sends results to the channel
func (a *DeepgramAdapter) readLoop() {
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
			// check if context was cancelled (normal shutdown)
			select {
			case <-a.ctx.Done():
				return
			default:
			}

			// check if we're finalizing - normal close after finalize is expected
			a.mu.Lock()
			finalizing := a.finalizing
			a.mu.Unlock()

			if finalizing && websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				a.finalizeDone <- nil
				return
			}
			fatal := NewFatalTranscriptionError(fmt.Errorf("websocket read: %w; recording cannot be resumed without audio loss", err))
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: fatal})
			if finalizing {
				a.finalizeDone <- fatal
			}
			a.cancel()
			return
		}

		// parse message
		var resp deepgramWSResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: NewFatalTranscriptionError(fmt.Errorf("deepgram: invalid server response: %w", err))})
			a.cancel()
			return
		}

		// handle different message types
		switch resp.Type {
		case "Metadata":
			if resp.Metadata != nil {
				log.Printf("deepgram: session started, request_id=%s, model=%s",
					resp.Metadata.RequestID, resp.Metadata.ModelInfo.Name)
			}

		case "Results":
			// transcription result
			if resp.Channel != nil && len(resp.Channel.Alternatives) > 0 {
				transcript := resp.Channel.Alternatives[0].Transcript
				if transcript != "" {
					isFinal := resp.IsFinal || resp.SpeechFinal
					if isFinal {
						log.Printf("deepgram: final: %q", transcript)
					}
					emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Text: transcript, IsFinal: isFinal})
				}
			}

		case "Error":
			details := []string{resp.Code, resp.Variant, resp.Description}
			if resp.Error != nil {
				details = append(details, resp.Error.Type, resp.Error.Message, resp.Error.Description)
			}
			errMsg := strings.TrimSpace(strings.Join(details, " "))
			if errMsg == "" {
				errMsg = "server reported a transcription error"
			}
			emitStreamingResult(a.ctx, a.resultsCh, TranscriptionResult{Error: NewFatalTranscriptionError(fmt.Errorf("deepgram: %s", errMsg))})
			a.cancel()
			return

		case "UtteranceEnd":
			log.Printf("deepgram: utterance end detected")

		case "SpeechStarted":
			log.Printf("deepgram: speech started")

		default:
			log.Printf("deepgram: unknown message type: %s", resp.Type)
		}
	}
}

// SendChunk sends audio data to the WebSocket
// Deepgram expects raw binary audio data, not base64 encoded
func (a *DeepgramAdapter) SendChunk(audio []byte) error {
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

	// send raw binary audio (not base64)
	a.mu.Lock()
	err := streamingWriteDeadline(a.ctx, conn)
	if err == nil {
		err = conn.WriteMessage(websocket.BinaryMessage, audio)
	}
	a.mu.Unlock()

	if err != nil {
		return NewFatalTranscriptionError(fmt.Errorf("websocket write: %w", err))
	}

	return nil
}

// Results returns the channel for receiving transcription results
func (a *DeepgramAdapter) Results() <-chan TranscriptionResult {
	return a.resultsCh
}

// Finalize sends a CloseStream message to signal end of audio and waits for final results
func (a *DeepgramAdapter) Finalize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
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

	// drain any previous finalize signals
	select {
	case <-a.finalizeDone:
	default:
	}

	// Only the terminal normal close acknowledges CloseStream.
	a.mu.Lock()
	a.finalizing = true
	a.mu.Unlock()

	// send CloseStream message
	msg := deepgramCloseStream{Type: "CloseStream"}

	a.mu.Lock()
	err := writeStreamingJSON(a.ctx, conn, msg)
	a.mu.Unlock()

	if err != nil {
		log.Printf("deepgram: finalize write error: %v", err)
		return fmt.Errorf("finalize write: %w", err)
	}

	log.Printf("deepgram: sent CloseStream, waiting for final transcript")

	// wait for final result or timeout
	select {
	case err := <-a.finalizeDone:
		log.Printf("deepgram: finalize complete")
		return err
	case <-ctx.Done():
		log.Printf("deepgram: finalize timeout")
		return ctx.Err()
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
}

// Close gracefully closes the WebSocket connection
func (a *DeepgramAdapter) Close() error {
	a.mu.Lock()

	if !a.started {
		a.mu.Unlock()
		return nil
	}

	// Treat shutdown as intentional rather than a disconnected session.
	a.finalizing = true

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

	log.Printf("deepgram: closed")
	return nil
}
