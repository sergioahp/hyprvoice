# idea-e: three-tier transcription cascade

Three layers run simultaneously on the same audio:

- **Tier 1 (instant draft)** — Mistral Voxtral realtime WebSocket (`voxtral-mini-transcribe-realtime-2602`). Token-by-token partials appear as the speaker talks; first word in under 1 second.
- **Tier 2 (refinement)** — `gpt-4o-mini-transcribe` called every `REFINE_INTERVAL` (default 4 s) on all audio since the last commit. Replaces the rough Voxtral draft with a cleaner version.
- **Tier 3 (30 s commit)** — `gpt-4o-transcribe` called once every `COMMIT_INTERVAL` (default 30 s). Authoritative text is locked into `committed`; draft resets to the tail after that boundary. A final tier-3 call fires on `stop` for any remaining audio.

Net effect: instant words (Voxtral) -> cleaner words (mini every 4 s) -> locked-in authoritative words every 30 s, so a long dictation is largely finalized before you stop.

## Run

```bash
# terminal 1
cd idea-e-voxtral-mini-big30s
nix develop /path/to/hyprvoice --command go run . 2>impl.log | \
  nix develop /path/to/hyprvoice --command go run ../visualizer

# terminal 2
socat - UNIX-CONNECT:idea-e-voxtral-mini-big30s/control.sock
# then: replay test-long.wav   (58 s clip, exercises the 30 s commit)
#       replay ../sample.wav   (16 s clip, exercises final-only path)
#       stop
#       mic                    (live microphone)
#       quit
```

## Knobs

| Constant | Default | Effect |
|---|---|---|
| `refineInterval` | 4 s | How often gpt-4o-mini-transcribe refines the draft |
| `commitInterval` | 30 s | How often gpt-4o-transcribe commits the authoritative text |
| `minAudioBytes` | 0.5 s | Minimum audio before any transcription call |

## API keys required

`OPENAI_API_KEY` and `MISTRAL_API_KEY` must be set in the environment.

## Voxtral WebSocket protocol

- Endpoint: `wss://api.mistral.ai/v1/audio/transcriptions/realtime?model=voxtral-mini-transcribe-realtime-2602`
- Auth: `Authorization: Bearer <MISTRAL_API_KEY>` header
- Send: `{"type":"session.update","session":{"audio_format":{"encoding":"pcm_s16le","sample_rate":16000}}}`
- Send audio: `{"type":"input_audio.append","audio":"<base64 pcm_s16le>"}` (max 262144 bytes decoded per message)
- Receive deltas: `{"type":"transcription.text.delta","text":"..."}` (incremental tokens)
- Receive final: `{"type":"transcription.done","text":"..."}` (on input_audio.end)
