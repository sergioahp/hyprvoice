package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type AudioFrame struct {
	Data      []byte
	Timestamp time.Time
}

type Config struct {
	SampleRate        int
	Channels          int
	Format            string
	BufferSize        int
	Device            string
	ChannelBufferSize int
	Timeout           time.Duration
}

// Recorder interface for audio recording
type Recorder interface {
	Start(ctx context.Context) (<-chan AudioFrame, <-chan error, error)
	Stop()
	IsRecording() bool
}

type recorder struct {
	config    Config
	recording atomic.Bool

	mu     sync.Mutex // guards the current capture session
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
}

func NewRecorder(config Config) Recorder {
	return &recorder{config: config}
}

func (r *recorder) IsRecording() bool {
	return r.recording.Load()
}

func (r *recorder) Start(ctx context.Context) (<-chan AudioFrame, <-chan error, error) {
	if r.recording.Load() {
		return nil, nil, fmt.Errorf("already recording")
	}

	if err := r.validateConfig(); err != nil {
		return nil, nil, err
	}

	recordingCtx, cancel := context.WithCancel(ctx)

	frameCh := make(chan AudioFrame, r.config.ChannelBufferSize)
	errCh := make(chan error, 1)
	readyCh := make(chan error, 1)

	r.mu.Lock()
	if r.recording.Load() {
		r.mu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf("already recording")
	}
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	r.recording.Store(true)
	r.mu.Unlock()
	go r.captureLoop(recordingCtx, frameCh, errCh, readyCh, done)

	// A spawned process is not yet a live microphone. Keep the first frame in
	// frameCh and report readiness only after capture actually produces PCM.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var err error
	select {
	case err = <-readyCh:
		if err == nil {
			return frameCh, errCh, nil
		}
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = fmt.Errorf("timed out waiting for microphone audio")
	}
	cancel()
	<-done
	return nil, nil, err
}

func (r *recorder) Stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *recorder) captureLoop(ctx context.Context, frameCh chan<- AudioFrame, errCh chan<- error, readyCh chan<- error, done chan struct{}) {
	startedAt := time.Now()
	ready := false
	var captureErr error
	var capturedBytes, droppedBytes int64
	defer func() {
		if !ready {
			if captureErr == nil {
				captureErr = fmt.Errorf("recording ended before receiving microphone audio")
			}
			readyCh <- captureErr
		}
		log.Printf("Recording: captured_bytes=%d dropped_bytes=%d", capturedBytes, droppedBytes)
		close(frameCh)
		close(errCh)
		r.mu.Lock()
		r.recording.Store(false)
		r.cancel = nil
		r.cmd = nil
		close(done)
		r.mu.Unlock()
	}()

	cmd := exec.CommandContext(ctx, "pw-record", r.buildPwRecordArgs()...)
	// Let pw-record flush its stdio buffer before falling back to a forced kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = time.Second
	stdout, writer, err := os.Pipe()
	if err != nil {
		captureErr = fmt.Errorf("create stdout pipe: %w", err)
		r.emitErr(errCh, captureErr)
		r.requestCancel()
		return
	}
	defer stdout.Close()
	cmd.Stdout = writer
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		writer.Close()
		captureErr = fmt.Errorf("start pw-record: %w", err)
		r.emitErr(errCh, captureErr)
		r.requestCancel()
		return
	}
	writer.Close()
	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()
	// Using our own pipe lets Wait run concurrently with reads. WaitDelay then
	// also bounds shutdown if the recorder ignores SIGINT and keeps stdout open.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	defer func() {
		r.requestCancel()
		<-waitCh
		if stderr.Len() > 0 {
			log.Printf("Recording stderr: %s", bytes.TrimSpace(stderr.Bytes()))
		}
	}()

	var pending []byte
	for {
		buffer := make([]byte, r.config.BufferSize+len(pending))
		copy(buffer, pending)
		n, readErr := stdout.Read(buffer[len(pending):])
		capturedBytes += int64(n)
		total := n + len(pending)
		aligned := total - total%2
		pending = buffer[aligned:total]
		if aligned > 0 {
			frame := AudioFrame{Data: buffer[:aligned], Timestamp: time.Now()}
			select {
			case frameCh <- frame:
				if !ready {
					ready = true
					log.Printf("Recording: microphone ready after %d ms", time.Since(startedAt).Milliseconds())
					readyCh <- nil
				}
			default:
				droppedBytes += int64(aligned)
				captureErr = fmt.Errorf("audio buffer full: lost %d bytes; refusing incomplete recording", aligned)
				r.emitErr(errCh, captureErr)
				r.requestCancel()
				return
			}
		}
		if readErr != nil {
			if len(pending) > 0 {
				captureErr = fmt.Errorf("recording ended with an incomplete PCM sample")
				r.emitErr(errCh, captureErr)
				return
			}
			if ctx.Err() != nil {
				return
			}
			if errors.Is(readErr, io.EOF) {
				captureErr = fmt.Errorf("pw-record stopped unexpectedly")
			} else {
				captureErr = fmt.Errorf("read audio: %w", readErr)
			}
			r.emitErr(errCh, captureErr)
			return
		}
	}
}

func (r *recorder) requestCancel() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *recorder) emitErr(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
	}
	log.Printf("Recording error: %v", err)
}

func (r *recorder) buildPwRecordArgs() []string {
	args := []string{
		"--raw", // downstream adapters expect PCM, not an audio container header
		"--format", r.config.Format,
		"--rate", strconv.Itoa(r.config.SampleRate),
		"--channels", strconv.Itoa(r.config.Channels),
		"-", // stdout
	}
	if r.config.Device != "" {
		args = append(args, "--target", r.config.Device)
	}
	return args
}

func CheckPipeWireAvailable(ctx context.Context) error {
	if _, err := exec.LookPath("pw-record"); err != nil {
		return fmt.Errorf("pw-record not found: %w (install pipewire-tools)", err)
	}
	// Use a short timeout to avoid hangs on misconfigured systems.
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, "pw-cli", "info", "all")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("PipeWire not running or accessible: %w", err)
	}
	return nil
}

func (r *recorder) validateConfig() error {
	if r.config.SampleRate <= 0 {
		return fmt.Errorf("invalid SampleRate: %d", r.config.SampleRate)
	}
	if r.config.Channels <= 0 {
		return fmt.Errorf("invalid Channels: %d", r.config.Channels)
	}
	if r.config.BufferSize <= 0 {
		return fmt.Errorf("invalid BufferSize: %d", r.config.BufferSize)
	}
	if r.config.ChannelBufferSize <= 0 {
		return fmt.Errorf("invalid ChannelBufferSize: %d", r.config.ChannelBufferSize)
	}
	if r.config.Format == "" {
		return fmt.Errorf("invalid Format: empty")
	}
	if err := ValidateAudioFormat(r.config.SampleRate, r.config.Channels, r.config.Format); err != nil {
		return err
	}
	// For s16, sample frame size is 2 bytes per sample per channel.
	if r.config.Format == "s16" {
		frameBytes := 2 * r.config.Channels
		if r.config.BufferSize%frameBytes != 0 {
			log.Printf("Recording: BufferSize %d not aligned to frame size %d; retaining partial samples between reads",
				r.config.BufferSize, frameBytes)
		}
	}
	return nil
}

// ValidateAudioFormat enforces the PCM format declared by all transcription adapters.
func ValidateAudioFormat(sampleRate, channels int, format string) error {
	if sampleRate != 16000 || channels != 1 || format != "s16" {
		return fmt.Errorf("transcription requires recording.sample_rate=16000, recording.channels=1 and recording.format=s16 (got %d, %d, %s)", sampleRate, channels, format)
	}
	return nil
}
