#!/usr/bin/env bash
# Launch the realtime-lab TUI. `go run .` must run from INSIDE the tui module dir
# (realtime-lab/tui has its own go.mod, separate from the parent hyprvoice module).
repo_root="$(cd "$(dirname "$0")/.." && pwd)" || exit 1
cd "$repo_root/realtime-lab/tui" || exit 1
exec nix develop "$repo_root" --command go run .
