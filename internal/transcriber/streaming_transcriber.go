package transcriber

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/leonardotrapani/hyprvoice/internal/recording"
)

// StreamingTranscriber wraps a StreamingAdapter and implements the Transcriber interface.
// It streams audio chunks to the adapter in real-time and accumulates transcription results.
type StreamingTranscriber struct {
	adapter  StreamingAdapter
	language string

	// accumulated final text
	finalText strings.Builder
	mu        sync.Mutex
	fatalErr  error

	// coordination
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	lifecycleMu sync.Mutex
	started     bool
	stopOnce    sync.Once
	stopErr     error
	sendDone    chan struct{}
	errCh       chan error
	finishing   atomic.Bool
}

func NewStreamingTranscriber(adapter StreamingAdapter, language string) *StreamingTranscriber {
	return &StreamingTranscriber{
		adapter:  adapter,
		language: language,
	}
}

func (t *StreamingTranscriber) Start(ctx context.Context, frameCh <-chan recording.AudioFrame) (<-chan error, error) {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.started {
		return nil, fmt.Errorf("transcriber already started")
	}
	t.started = true
	t.ctx, t.cancel = context.WithCancel(ctx)

	if err := t.adapter.Start(t.ctx, t.language); err != nil {
		t.cancel()
		return nil, err
	}

	errCh := make(chan error, 2)
	t.errCh = errCh
	t.sendDone = make(chan struct{})

	// goroutine 1: read audio frames and send to adapter
	t.wg.Add(1)
	go t.sendAudio(frameCh, errCh)

	// goroutine 2: read results from adapter and accumulate
	t.wg.Add(1)
	go t.receiveResults(errCh)

	return errCh, nil
}

func (t *StreamingTranscriber) sendAudio(frameCh <-chan recording.AudioFrame, errCh chan<- error) {
	defer t.wg.Done()
	defer close(t.sendDone)

	for {
		select {
		case <-t.ctx.Done():
			return
		case frame, ok := <-frameCh:
			if !ok {
				return
			}
			if err := t.adapter.SendChunk(frame.Data); err != nil {
				if t.ctx.Err() != nil {
					return
				}
				// A failed chunk is missing audio, even if a later chunk could
				// succeed. Do not inject a transcript with an unnoticed gap.
				fatal := NewFatalTranscriptionError(err)
				if t.setFatalErr(fatal) {
					select {
					case errCh <- fatal:
					default:
					}
				}
				t.cancel()
				return
			}
		}
	}
}

func (t *StreamingTranscriber) receiveResults(errCh chan<- error) {
	defer t.wg.Done()

	resultsCh := t.adapter.Results()
	for {
		select {
		case <-t.ctx.Done():
			// context cancelled, drain any remaining results before exiting
			t.drainRemainingResults(resultsCh, errCh)
			return
		case result, ok := <-resultsCh:
			if !ok {
				if !t.finishing.Load() && t.ctx.Err() == nil {
					t.processResult(TranscriptionResult{Error: NewFatalTranscriptionError(fmt.Errorf("transcription stream closed before finalization"))}, errCh)
				}
				return
			}
			t.processResult(result, errCh)
		}
	}
}

func (t *StreamingTranscriber) processResult(result TranscriptionResult, errCh chan<- error) {
	if result.Error != nil {
		if IsFatalTranscriptionError(result.Error) {
			if t.setFatalErr(result.Error) {
				select {
				case errCh <- result.Error:
				default:
				}
			}
			log.Printf("streaming transcriber: result error: %v", result.Error)
			if t.cancel != nil {
				t.cancel()
			}
			return
		}
		select {
		case errCh <- result.Error:
		default:
		}
		log.Printf("streaming transcriber: result error: %v", result.Error)
		return
	}
	if result.IsFinal && result.Text != "" {
		t.mu.Lock()
		if t.finalText.Len() > 0 {
			t.finalText.WriteString(" ")
		}
		t.finalText.WriteString(result.Text)
		t.mu.Unlock()
	}
}

func (t *StreamingTranscriber) drainRemainingResults(resultsCh <-chan TranscriptionResult, errCh chan<- error) {
	// Close stops the adapter reader and closes resultsCh. Drain until then so
	// buffered final results and fatal errors survive cancellation.
	for result := range resultsCh {
		t.processResult(result, errCh)
	}
}

func (t *StreamingTranscriber) Stop(ctx context.Context) error {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.sendDone == nil {
		return nil
	}
	t.stopOnce.Do(func() {
		// The recorder must close frameCh before Stop. Finalizing before sendAudio
		// drains it can commit only a prefix of the recording.
		select {
		case <-t.sendDone:
		case <-ctx.Done():
			t.stopErr = ctx.Err()
			t.cancel()
		case <-t.ctx.Done():
			t.stopErr = t.ctx.Err()
		}
		if t.stopErr == nil && t.ctx.Err() == nil {
			t.finishing.Store(true)
			t.stopErr = t.adapter.Finalize(ctx)
		}
		t.cancel()
		closeErr := t.adapter.Close()
		t.wg.Wait()
		close(t.errCh)
		if fatalErr := t.getFatalErr(); fatalErr != nil {
			t.stopErr = fatalErr
		} else if t.stopErr == nil {
			t.stopErr = closeErr
		}
	})
	return t.stopErr
}

func (t *StreamingTranscriber) GetFinalTranscription() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fatalErr != nil {
		return "", t.fatalErr
	}
	return t.finalText.String(), nil
}

func (t *StreamingTranscriber) setFatalErr(err error) bool {
	if err == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fatalErr != nil {
		return false
	}
	t.fatalErr = err
	return true
}

func (t *StreamingTranscriber) getFatalErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fatalErr
}
