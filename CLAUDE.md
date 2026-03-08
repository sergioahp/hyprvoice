# Fork context

This is a fork of `LeonardoTrapani/hyprvoice` maintained at `sergioahp/hyprvoice`.

## Custom additions (branch: feature/custom)

### Nix flake & Home Manager module (`flake.nix`, `hm-module.nix`)
A production Nix flake exposing a Home Manager module. Key options:
- `services.hyprvoice.enable` — installs the binary and systemd user service
- `services.hyprvoice.autoStart` — whether to start on login (compositor-started)
- `services.hyprvoice.environmentFile` — path to a file with secrets (e.g. API keys)

The systemd service uses `ExecStartPre` to wait for `WAYLAND_DISPLAY` and is ordered after the compositor via `After=graphical-session.target`.

### Persistent Dunst notifications (`internal/notify/notify.go`)
Recording is modelled as a stateful operation. All state transitions replace the same Dunst notification (replace ID `9999`) instead of spawning new ones:
- Recording started → `🎤 Recording...` (persistent)
- Transcribing → `⏳ Transcribing...` (persistent, replaces above)
- Complete → `✅ Complete` (5s timeout, replaces above)
- Cancelled/aborted → `❌ Cancelled` (5s timeout, replaces above)

The `RecordingCancelled()` / `MsgRecordingCancelled` message type must be preserved and wired into all cancel/abort paths in `internal/daemon/daemon.go`.

## Important invariants

- **Do not change upstream's default config message strings** (e.g. `"Recording Started"`, `"Recording Ended... Transcribing"`). These are tested by upstream's `TestMessagesConfig_Resolve_Defaults`. Our persistent-notification behaviour lives entirely in `Desktop.Send()` in `notify.go` (via `notify-send -r 9999`), not in the config default strings.
- The emoji (`🎤`, `⏳`, `✅`, `❌`) appear in the `notify-send` body arguments in `Desktop.Send()`, not in config defaults.

## When merging upstream

- Always preserve the Nix flake and HM module files — upstream does not have these.
- If upstream refactors `internal/notify/`, integrate the persistent-notification behaviour into whatever new structure they introduce. The key invariant: cancel and abort paths must replace the same notification ID as the recording-started notification.
- If upstream changes `internal/daemon/daemon.go`, make sure cancel/abort still calls the cancelled message type.
- Run `nix develop --command go build ./...` and `nix develop --command go test ./...` after resolving to verify nothing broke.
