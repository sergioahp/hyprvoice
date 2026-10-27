# idea-d: OpenAI realtime draft + gpt-4o-transcribe final

Streams mic/replay audio to OpenAI's GA Realtime transcription API for a live
draft, and makes one `gpt-4o-transcribe` batch call at `stop` for the
authoritative final. Implements the lab contract (stdout JSONL events + unix
control socket; see ../INTERFACE.md).

## Models / providers
- Draft: `gpt-4o-transcribe` over `wss://api.openai.com/v1/realtime?intent=transcription`
- Final: `gpt-4o-transcribe` batch (POST /v1/audio/transcriptions)
- Key: `OPENAI_API_KEY` (env)

## Run
```
nix develop /home/admin/code/go/hyprvoice --command go run . 2>impl.log | \
  nix develop /home/admin/code/go/hyprvoice --command go run ../visualizer
# other terminal:
socat - UNIX-CONNECT:control.sock   # type: replay   then: stop
```

## Important finding (the headline result of idea D)

OpenAI's realtime transcription is accurate ONLY when you commit a COMPLETE
utterance. We tested three commit strategies on the same byte-correct audio
(verified: the exact 24kHz PCM sent over the wire batch-transcribes perfectly):

- `server_vad` turn detection -> GARBAGE. Mis-segments into tiny turns; the model
  hallucinates unrelated words ("Oh my God", "Hello"), differing run to run.
- fixed ~2.5s manual commits -> GARBAGE on mid-phrase cuts ("it's about a meter
  away" -> "giveaway", "Estodolido"). Segments that happen to align with pauses
  are fine; the rest are not.
- single commit at `stop` (no turn_detection) -> PERFECT. The whole transcript
  streams back word-by-word as deltas in ~1s.

So this prototype uses `turn_detection: null` + one commit at stop. Consequence:
the draft is NOT live during speech (it appears word-by-word right after you
stop). For a genuinely live, accurate during-speech draft, use Voxtral realtime
(idea-a / idea-e), whose native streaming architecture transcribes correct words
incrementally.

## Protocol notes (GA realtime, current)
- Message type is `session.update` (the beta `transcription_session.update` is
  rejected: "Invalid value... Supported values are: 'session.update', ...").
- Audio format is a nested OBJECT: `{"type":"audio/pcm","rate":24000}`. pcm16
  requires 24kHz mono s16le, so 16kHz lab audio is resampled 16k->24k before send
  (the API rejects rate < 24000).
- Partials: `conversation.item.input_audio_transcription.delta` (.delta);
  completed: `...transcription.completed` (.transcript).

## Knobs
- transcription model in `wsConnect` (gpt-4o-transcribe / gpt-4o-mini-transcribe)
- final batch model in the stop path
