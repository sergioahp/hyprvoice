# idea-b: cheap HTTP polling draft

Post buffered PCM to `gpt-4o-mini-transcribe` on a ~1 Hz timer for live drafts,
then call `gpt-4o-transcribe` exactly once on the full buffer at stop for the
authoritative final. No WebSocket, no streaming — just repeated cheap batch HTTP.

**Models:** draft = `gpt-4o-mini-transcribe`, final = `gpt-4o-transcribe`

**Window strategy:** rolling tail — each draft poll sends at most the last 8 s of
audio (`TAIL_SECS`). For short recordings the full buffer is used. A pending draft
request blocks the next tick (no piling-up); skipped ticks are logged to stderr.

## How to run

```
# terminal 1
cd realtime-lab/idea-b-mini-poll
nix develop /home/admin/code/go/hyprvoice --command go run . 2>impl.log | \
  nix develop /home/admin/code/go/hyprvoice --command go run ../visualizer

# terminal 2
socat - UNIX-CONNECT:realtime-lab/idea-b-mini-poll/control.sock
# type: replay      (uses ../sample.wav)
# type: stop
```

## Knobs (constants in main.go)

| constant       | default | meaning                                       |
|----------------|---------|-----------------------------------------------|
| `pollInterval` | 900 ms  | draft poll cadence (<=1 Hz to respect limits) |
| `tailSecs`     | 8.0 s   | max audio sent per draft call                 |
| `replayChunk`  | 3200 B  | bytes fed per tick during replay (100 ms)     |

## Observed on sample.wav (~16 s)

- time-to-first-word: ~1.5 s (first poll at 900 ms + ~600 ms API latency)
- draft updates: ~10 over 16 s (some ticks skipped when API > 900 ms)
- final transcript: correct full-sentence output from strong model
