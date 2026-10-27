# idea-c: VAD-segmented pipeline with progressive strong lock-in

Energy-based voice-activity detection splits the incoming PCM stream into speech
segments. Each CLOSED segment is immediately finalized by `gpt-4o-transcribe`
(strong model, rate-limited to >= 1 s between calls) and appended to the
committed text. While a segment is OPEN, `gpt-4o-mini-transcribe` is polled
every ~1 s on just that segment's audio to show a volatile draft. On `stop`,
the last open segment is finalized and the full stitched text is emitted as
`final`. By the time the user stops, most segments are already locked in, so
end-latency is near-zero.

## Models used

| role | model |
|------|-------|
| live draft (~1 Hz) | `gpt-4o-mini-transcribe` |
| segment finalization | `gpt-4o-transcribe` (>= 1 s spacing enforced) |

## How to run

```sh
# terminal 1
cd idea-c-vad-segmented
nix develop /home/admin/code/go/hyprvoice --command go run . 2>impl.log | \
  /path/to/realtime-lab/visualizer/visualizer

# terminal 2
socat - UNIX-CONNECT:idea-c-vad-segmented/control.sock
# then type: replay        (uses ../sample.wav)
#            mic           (live microphone via pw-record)
#            stop          (finalize and emit final)
#            status        (show current state)
#            quit
```

## VAD knobs (in main.go constants)

| constant | default | meaning |
|----------|---------|---------|
| `defaultRMSThreshold` | 0.015 | normalized RMS below = silence; raise to 0.03-0.05 for noisy rooms |
| `defaultMinSilence` | 600ms | continuous silence >= this closes the current segment |
| `defaultDraftPoll` | 1s | how often to call the draft model on the open segment |

The VAD is purely energy-based (no ML): RMS of each 100 ms chunk vs. threshold.
A short silence gap mid-sentence is accumulated into the segment; only a silence
>= `defaultMinSilence` triggers a boundary.

## Observed behavior on sample.wav (~16 s, real speech)

- **Segments detected**: 3
- **Time-to-first-word**: ~1.6 s from start of recording
- **End-latency after `stop`**: near-zero (all segments already committed)
- **Final transcript**: "The camera is not directly in front of me, it's about a meter away. which It's totally doable for day-to-day use."
