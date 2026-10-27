package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/bus"
	"github.com/leonardotrapani/hyprvoice/internal/config"
	"github.com/leonardotrapani/hyprvoice/internal/notify"
	"github.com/leonardotrapani/hyprvoice/internal/pipeline"
)

type Daemon struct {
	mu        sync.RWMutex
	controlMu sync.Mutex
	notifier  notify.Notifier
	configMgr *config.Manager

	ctx    context.Context
	cancel context.CancelFunc

	pipeline       pipeline.Pipeline
	contextSession bool // current pipeline was started with scrollback context

	wg        sync.WaitGroup
	monitorWG sync.WaitGroup
}

func New() (*Daemon, error) {
	configMgr, err := config.NewManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create config manager: %w", err)
	}

	conf := configMgr.GetConfig()
	ctx, cancel := context.WithCancel(context.Background())

	// force desktop notifications when legacy config so user sees the onboarding prompt
	notifType := conf.Notifications.Type
	if configMgr.IsLegacy() {
		notifType = "desktop"
	}

	d := &Daemon{
		notifier:  notify.NewNotifier(notifType, conf.Notifications.Messages.Resolve()),
		configMgr: configMgr,
		ctx:       ctx,
		cancel:    cancel,
	}

	return d, nil
}

func (d *Daemon) onConfigReload() {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	log.Printf("Config reloaded, restarting pipeline")
	d.stopPipeline()

	conf := d.configMgr.GetConfig()

	d.mu.Lock()
	d.notifier = notify.NewNotifier(conf.Notifications.Type, conf.Notifications.Messages.Resolve())
	d.mu.Unlock()

	d.sendNotify(notify.MsgConfigReloaded)
}

func (d *Daemon) sendNotify(mt notify.MessageType) {
	d.mu.RLock()
	notifier := d.notifier
	d.mu.RUnlock()
	notifier.Send(mt)
}

func (d *Daemon) sendError(message string) {
	d.mu.RLock()
	notifier := d.notifier
	d.mu.RUnlock()
	notifier.Error(message)
}

func (d *Daemon) status() pipeline.Status {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.pipeline == nil {
		return pipeline.Idle
	}
	return d.pipeline.Status()
}

func (d *Daemon) stopPipeline() {
	d.mu.Lock()
	p := d.pipeline
	d.pipeline = nil
	d.contextSession = false
	d.mu.Unlock()

	if p != nil {
		p.Stop()
	}
	d.monitorWG.Wait()
}

func (d *Daemon) Run() error {
	if err := bus.CheckExistingDaemon(); err != nil {
		return err
	}

	d.configMgr.SetOnConfigReload(d.onConfigReload)

	ln, err := bus.Listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	defer d.cancel()
	defer d.stopPipeline()

	if err := bus.CreatePidFile(); err != nil {
		return fmt.Errorf("failed to create PID file: %w", err)
	}
	defer bus.RemovePidFile()

	if err := d.configMgr.StartWatching(d.ctx); err != nil {
		log.Printf("Warning: failed to start config file watching: %v", err)
	}
	defer d.configMgr.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("Received signal %v, shutting down gracefully", sig)
			d.cancel()
		case <-d.ctx.Done():
		}
	}()

	go func() {
		<-d.ctx.Done()
		if err := ln.Close(); err != nil {
			log.Printf("Error closing listener: %v", err)
		}
	}()

	log.Printf("Daemon started, listening on socket")

	for {
		c, err := ln.Accept()
		if err != nil {
			if d.ctx.Err() != nil {
				log.Printf("Shutdown requested, waiting for connections to finish")
				d.wg.Wait()
				return nil
			}
			log.Printf("Accept error: %v", err)
			return fmt.Errorf("accept failed: %w", err)
		}
		d.wg.Add(1)
		go d.handle(c)
	}
}

func (d *Daemon) handle(c net.Conn) {
	defer c.Close()
	defer d.wg.Done()
	stopClose := context.AfterFunc(d.ctx, func() { c.Close() })
	defer stopClose()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(c)
	header, err := reader.ReadSlice('\n')
	line := string(header)
	if err != nil {
		log.Printf("Client read error: %v", err)
		fmt.Fprintf(c, "ERR read_error: %v\n", err)
		return
	}
	if len(line) == 0 {
		fmt.Fprint(c, "ERR empty\n")
		return
	}
	cmd := line[0]

	switch cmd {
	case 't':
		// Optional context body: "t <len>\n<body>" or plain "t\n"
		var contextPrompt string
		if lenStr := strings.TrimSpace(line[1:]); lenStr != "" {
			var err error
			contextPrompt, err = d.readBody(reader, line)
			if err != nil {
				fmt.Fprintf(c, "ERR read_body: %v\n", err)
				return
			}
		}
		d.toggle(contextPrompt)
		fmt.Fprint(c, "OK toggled\n")
	case 'c':
		d.cancelPipeline()
		fmt.Fprint(c, "OK cancelled\n")
	case 's':
		status := d.status()
		fmt.Fprintf(c, "STATUS status=%s\n", status)
	case 'v':
		fmt.Fprintf(c, "STATUS proto=%s\n", bus.ProtoVer)
	case 'q':
		fmt.Fprint(c, "OK quitting\n")
		d.cancel()
	default:
		log.Printf("Unknown command: %c", cmd)
		fmt.Fprintf(c, "ERR unknown=%q\n", cmd)
	}
}

func (d *Daemon) readBody(reader *bufio.Reader, header string) (string, error) {
	lenStr := strings.TrimSpace(header[1:])
	bodyLen, err := strconv.Atoi(lenStr)
	if err != nil || bodyLen < 0 || bodyLen > 1024*1024 {
		return "", fmt.Errorf("invalid body length %q", lenStr)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", err
	}
	return string(body), nil
}

func (d *Daemon) toggle(contextPrompt string) {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	if d.configMgr.IsLegacy() {
		d.sendError("Legacy config detected. Run: hyprvoice onboarding")
		return
	}
	conf := d.configMgr.GetConfig()
	switch d.status() {
	case pipeline.Idle:
		d.stopPipeline()
		var opts []pipeline.Option
		if contextPrompt != "" {
			opts = append(opts, pipeline.WithContextPrompt(contextPrompt))
		}
		p := pipeline.New(conf, opts...)
		p.Run(d.ctx)

		d.mu.Lock()
		d.pipeline = p
		d.contextSession = contextPrompt != ""
		d.mu.Unlock()

		d.monitorWG.Add(2)
		go func() { defer d.monitorWG.Done(); d.monitorPipelineErrors(p) }()
		go func() { defer d.monitorWG.Done(); d.monitorPipelineNotifications(p) }()

	case pipeline.Recording:
		d.stopPipeline()
		d.sendNotify(notify.MsgRecordingAborted)

	case pipeline.Transcribing:
		d.mu.RLock()
		p := d.pipeline
		d.mu.RUnlock()
		if p != nil {
			select {
			case p.GetActionCh() <- pipeline.Inject:
				log.Printf("Daemon: Sent inject action to pipeline")
			default:
				log.Printf("Daemon: Pipeline already has a pending action")
			}
		}

	case pipeline.Injecting, pipeline.Processing:
		d.stopPipeline()
		d.sendNotify(notify.MsgInjectionAborted)
	}
}

func (d *Daemon) cancelPipeline() {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	switch d.status() {
	case pipeline.Idle:
		log.Printf("Daemon: Cancel requested but pipeline is idle, ignoring")
	default:
		d.stopPipeline()
		d.sendNotify(notify.MsgOperationCancelled)
	}
}

func (d *Daemon) monitorPipelineErrors(p pipeline.Pipeline) {
	errorCh := p.GetErrorCh()
	for {
		select {
		case pipelineErr, ok := <-errorCh:
			if !ok {
				return
			}
			message := pipelineErr.Message

			if pipelineErr.Err != nil {
				message = fmt.Sprintf("%s: %v", message, pipelineErr.Err)
			}

			d.sendError(message)
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) monitorPipelineNotifications(p pipeline.Pipeline) {
	notifyCh := p.GetNotifyCh()
	for {
		select {
		case mt, ok := <-notifyCh:
			if !ok {
				return
			}
			d.sendNotify(mt)
		case <-d.ctx.Done():
			return
		}
	}
}
