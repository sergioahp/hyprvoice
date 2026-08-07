package injection

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type wtypeBackend struct{}

func NewWtypeBackend() Backend {
	return &wtypeBackend{}
}

func (w *wtypeBackend) Name() string {
	return "wtype"
}

func (w *wtypeBackend) Available() error {
	if _, err := exec.LookPath("wtype"); err != nil {
		return fmt.Errorf("wtype not found: %w (install wtype package)", err)
	}

	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("WAYLAND_DISPLAY not set - wtype requires Wayland session")
	}

	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return fmt.Errorf("XDG_RUNTIME_DIR not set - wtype requires proper session environment")
	}

	return nil
}

// wtype presses one key per character, measured at roughly 220 chars/s. A flat
// timeout means SIGKILL partway through a long transcription, which leaves half
// the text in the user's buffer with no way to undo it. Treat the configured
// timeout as a floor and scale the deadline with the text length instead.
const wtypeCharBudget = 10 * time.Millisecond

func wtypeTimeout(text string, configured time.Duration) time.Duration {
	if scaled := time.Duration(len([]rune(text))) * wtypeCharBudget; scaled > configured {
		return scaled
	}
	return configured
}

func (w *wtypeBackend) Inject(ctx context.Context, text string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wtypeTimeout(text, timeout))
	defer cancel()

	if err := w.Available(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "wtype", "--", text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wtype failed: %w", err)
	}

	return nil
}
