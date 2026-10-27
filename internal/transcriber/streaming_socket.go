package transcriber

import (
	"context"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

func bindStreamingSocket(ctx context.Context, conn *websocket.Conn) {
	// Closing the established socket interrupts reads and writes without waiting
	// for an adapter mutex held by a blocked writer.
	context.AfterFunc(ctx, func() { conn.Close() })
}

func streamingWriteDeadline(ctx context.Context, conn *websocket.Conn) error {
	if conn == nil {
		return fmt.Errorf("streaming connection is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return conn.SetWriteDeadline(deadline)
}

func writeStreamingJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	if err := streamingWriteDeadline(ctx, conn); err != nil {
		return err
	}
	return conn.WriteJSON(value)
}

func emitStreamingResult(ctx context.Context, ch chan<- TranscriptionResult, result TranscriptionResult) {
	select {
	case ch <- result:
	case <-ctx.Done():
	}
}
