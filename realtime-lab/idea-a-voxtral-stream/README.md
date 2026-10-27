# idea-a: Mistral Voxtral realtime draft + gpt-4o-transcribe final

**Draft path**: one persistent WebSocket to `wss://api.mistral.ai/v1/audio/transcriptions/realtime`
(model `voxtral-mini-transcribe-realtime-2602`, billed per minute). Streams 16kHz mono PCM16
directly (no resampling). Voxtral emits `transcription.text.delta` events as words arrive ->
shown live as `draft`, promoted to `committed` on `transcription.done`.

**Final path**: at `stop`, a single batch POST to OpenAI `/v1/audio/transcriptions` with
`gpt-4o-transcribe` on the full buffered PCM -> emitted as the authoritative `final`.

**Models/providers**: Mistral (Voxtral realtime) for live draft + OpenAI (gpt-4o-transcribe) for final.

## Run

```
cd idea-a-voxtral-stream
nix develop /home/admin/code/go/hyprvoice --command go run . 2>impl.log | \
  nix develop /home/admin/code/go/hyprvoice --command go run ../visualizer
```

In another terminal:
```
socat - UNIX-CONNECT:idea-a-voxtral-stream/control.sock
# type: replay        (feeds ../sample.wav at 1x speed)
#       stop          (finalizes + calls gpt-4o-transcribe)
#       mic           (live microphone via pw-record)
#       status / quit
```

## Knobs

- `streamDelayMs` (const in main.go): `target_streaming_delay_ms` for Voxtral, 80..2400ms.
  Lower = faster first word but less accurate (default 480ms).
- Chunk size: 640 bytes / 20ms chunks for replay; 3200 bytes / 100ms for mic.

## Protocol notes

Mistral realtime WebSocket wire format discovered via API error responses:
- `session.update` requires a nested `session` object (`audio_format`, `target_streaming_delay_ms`).
  Model goes only in the URL query param.
- Audio is sent as `input_audio.append` (not `input_audio_buffer.append`).
- End-of-stream is `input_audio.end` (not `input_audio_buffer.commit`).
- Server emits `transcription.text.delta` (not `transcription.delta`) for incremental text.

## Observed performance (sample.wav, 15.9s)

- Time to first word: 1719ms (480ms Voxtral streaming delay + network)
- Draft updates: 32 live token-by-token updates, smooth word growth in the visualizer
- Voxtral draft/committed: "The camera is not directly in front of me, it's about a meter away. Which is totally doable for day to day, day to day use."
- OpenAI final (authoritative): "The camera is not directly in front of me, it's about a meter away." in ~615ms
- Note: Voxtral emits `transcription.done` only at the end (not mid-stream segments for this sample). The `transcription.segment` event was not triggered.
