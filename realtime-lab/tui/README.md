# realtime-lab TUI

Interactive controller + live transcript viewer for the prototypes. It launches a
prototype as a child process, renders its JSONL event stream live (solid
committed text + dim volatile draft), and sends commands to the prototype's unix
control socket on keypress. Replaces the old `impl | visualizer` + `socat` combo.

## Run
```
realtime-lab/tui.sh
# or, from realtime-lab/:
nix develop /home/admin/code/go/hyprvoice --command go run ./tui
```
Run it in a real terminal (it's full-screen). Inside the devshell it rebuilds the
selected prototype fresh on each launch; outside the devshell it falls back to the
already-built prototype binaries.

## Keys
- `1`-`5`  select / (re)launch that prototype
- `tab`    next prototype
- `r`      replay ../sample.wav
- `R`      replay test-long.wav (idea-e: triggers a 30s commit)
- `m`      mic (live capture from the default PipeWire source)
- `s`      stop (finalize -> final transcript)
- `c`      clear the transcript view
- `l`      toggle log pane (stderr / info / errors)
- `?`      help
- `q`      quit

## Headless self-test (no TTY, for verifying the plumbing)
```
nix develop /home/admin/code/go/hyprvoice --command go run ./tui -- --selftest idea-b
```
Spawns the idea, replays the sample, prints committed/final events, then stops.
