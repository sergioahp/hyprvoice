# Realtime draft+strong transcription lab - interface contract

Goal: prototype several ways to mix a FAST/cheap "draft" transcription (shown
live, words may change as they are refined) with a STRONG/expensive model that
produces the authoritative final text. Each prototype is a self-contained Go
program in its own dir. All prototypes speak the SAME two interfaces below so
they are interchangeable and directly comparable through one shared visualizer.

This is an experimental lab, preserved with its samples and historical logs.
Optimize for "works and is comparable", not for production polish. Generated
binaries and new runtime outputs are ignored.

```
  control socket (you -> impl)            event stream stdout (impl -> visualizer)
  +------------------+    commands        +------------------+    JSONL
  |  you / socat     | -----------------> |   prototype      | -----------------> visualizer
  +------------------+                    +------------------+
```

## 1. Event stream (stdout, JSONL)  -- impl -> visualizer

The prototype writes ONE JSON object per line to **stdout**. Nothing else goes
to stdout (all logs/diagnostics go to **stderr**). The visualizer reads these.

Event types:

| type        | fields                | meaning                                                   |
|-------------|-----------------------|-----------------------------------------------------------|
| `status`    | `state`               | "recording" / "transcribing" / "idle"                     |
| `draft`     | `text`                | FULL volatile tail being refined (replace semantics)      |
| `committed` | `text`                | FULL authoritative text confirmed so far (replace)        |
| `final`     | `text`                | FULL final transcript; marks the utterance done           |
| `info`      | `text`                | human note, scrolls above the live block                  |
| `error`     | `text`                | error note, scrolls above the live block                  |
| `reset`     | -                     | clear state for a new utterance                            |

REPLACE SEMANTICS: `draft`, `committed`, and `final` each carry the FULL current
text every time, not a delta. To revise earlier words, just send the new best
text. The visualizer renders `committed` (normal) + a space + `draft` (dimmed).

NON-OVERLAP RULE: `committed` is the stable prefix; `draft` is ONLY the tail that
comes AFTER `committed`. They must not repeat the same words. When the strong
model confirms more text, grow `committed` and send a `draft` that is just the
remaining unconfirmed tail (or `""`).

Examples:
```
{"type":"status","state":"recording"}
{"type":"draft","text":"so the next thing we"}
{"type":"draft","text":"so the next thing we want to do"}
{"type":"committed","text":"So the next thing we want to do"}
{"type":"draft","text":"is wire up the socket"}
{"type":"final","text":"So the next thing we want to do is wire up the socket."}
```

Emit promptly: the visualizer measures time-to-first-word and counts updates, so
latency differences between prototypes show up in its footer.

## 2. Control socket (unix stream)  -- you -> impl

On startup the prototype listens on a unix stream socket and prints its path to
**stderr**, e.g. `control socket: /abs/path/control.sock`. Default path:
`./control.sock` in the prototype's own dir. Commands are newline-delimited
plain text; the impl may reply with one or more text lines (acks/errors) on the
same connection. Drive it with: `socat - UNIX-CONNECT:control.sock`.

Required commands:

| command        | effect                                                            |
|----------------|-------------------------------------------------------------------|
| `replay [path]`| feed a WAV at ~1x realtime (default `../sample.wav`); start fresh  |
| `mic`          | capture live from the default PipeWire source (see below)         |
| `stop`         | stop capture/replay, run finalization, emit `final`               |
| `status`       | reply with current state                                          |
| `quit`         | shut down cleanly                                                 |

`replay` is the primary path for evaluation: it lets every prototype run on the
EXACT same audio. `mic` is for live testing.

## 3. Audio

Internal format is **16 kHz, mono, signed 16-bit little-endian PCM** (s16le).

- `replay [path]`: read a WAV (the provided `../sample.wav` is 16 kHz mono s16),
  and push it through your pipeline in small chunks paced at ~1x wall-clock so
  partial behavior matches live use. Do NOT dump the whole file at once.
- `mic`: shell out to
  `pw-record --format s16 --rate 16000 --channels 1 -`
  and read raw PCM from its stdout (this is exactly how the parent hyprvoice app
  records). Stop it by killing the process / cancelling its context.

Realtime APIs that want PCM frames generally accept s16le @ 16 kHz directly. If a
specific API needs a different container/rate, convert with `ffmpeg` (available).

## 3b. Generating test audio with TTS (optional but useful)

`../sample.wav` is only ~16s of real speech. If your idea needs LONGER audio
(e.g. to exercise a periodic commit cadence) or specific content, you may
synthesize speech with a text-to-speech model and save it as a 16 kHz mono s16
WAV for `replay`:

- OpenAI TTS: `tts-1` or `gpt-4o-mini-tts` (POST /v1/audio/speech, `OPENAI_API_KEY`).
- Mistral also offers a Voxtral TTS model (`MISTRAL_API_KEY`).
- Convert/resample whatever you get to 16 kHz mono s16 with `ffmpeg`:
  `ffmpeg -i out.mp3 -ar 16000 -ac 1 -c:a pcm_s16le test-long.wav`

There are also pre-made TTS samples in `/home/admin/code/go/hyprvoice/testdata/`
(tts_*.wav). Keep generated test WAVs inside your own idea dir.

## 4. How it is run / evaluated

In one terminal:
```
cd <your-idea-dir>
nix develop /home/admin/code/go/hyprvoice --command go run . 2>impl.log | \
  nix develop /home/admin/code/go/hyprvoice --command go run ../visualizer
```
In another terminal:
```
socat - UNIX-CONNECT:<your-idea-dir>/control.sock
# then type:  replay      (uses ../sample.wav)
#             stop
```
The grader watches the visualizer block: time-to-first-word, how readable the
live draft is, and whether the final is correct.

## 5. Build / environment (NixOS)

- `go` is ONLY available inside the repo devshell. Build/run with:
  `nix develop /home/admin/code/go/hyprvoice --command go <args>`
- Each prototype is its own Go module (`go mod init <name>`); keep external deps
  minimal. A WebSocket client lib is fine (e.g. `nhooyr.io/websocket` or
  `github.com/coder/websocket`); `go mod tidy` will fetch it (network is up).
- Reference implementation patterns live in the parent repo at
  `/home/admin/code/go/hyprvoice/internal/transcriber/` (existing OpenAI batch,
  OpenAI-realtime, deepgram, elevenlabs adapters) and `internal/recording/`.

## 6. API keys (already set as environment variables)

Read these from the environment; do NOT hardcode or print them.

- `OPENAI_API_KEY`     - OpenAI (gpt-4o-transcribe, gpt-4o-mini-transcribe, gpt-4o-realtime-preview)
- `MISTRAL_API_KEY`    - Mistral (voxtral realtime + voxtral batch transcribe)
- `OPENROUTER_API_KEY` - OpenRouter (proxy to many models, OpenAI-compatible)

## 7. Cost / rate constraints (IMPORTANT)

- The STRONG model `gpt-4o-transcribe` must be called **at most once per second**.
  Prefer calling it once at the end, or only at speech pauses - never on a fixed
  sub-second timer.
- For frequent/repeated refinement calls, use the cheap `gpt-4o-mini-transcribe`
  (or a streaming session), NOT the big model.
- A true streaming session (voxtral-realtime / gpt-4o-realtime) is billed per
  minute of one continuous connection - that is fine and is NOT "calling every
  second".

## 8. What to deliver in your dir

1. A runnable Go program implementing both interfaces above for your assigned idea.
2. A short `README.md`: the idea in 3-5 lines, which models/providers it uses,
   how to run it, and any knobs (chunk size, pause threshold, etc.).
3. Verify it yourself before finishing: `go build`, then `replay` on
   `../sample.wav` end to end and confirm the visualizer shows a live draft that
   converges to a correct final. Note the time-to-first-word you observed.
