# Recording cutoff investigation

## Findings

The code had a real interval in which the desktop said recording had started
but the microphone had not started delivering audio. The daemon launched the
pipeline asynchronously and immediately sent the notification. Recorder.Start
then ran `pw-cli info all` before spawning `pw-record`, and returned without
waiting for any samples. There was no pre-roll buffer. Speech before capture
started could therefore be missing from the uploaded recording.

The installed daemon uses batch `gpt-transcribe`, 16000 Hz, mono, s16, and the
Webcam C270 source. The batch collector appends every received frame and the
OpenAI adapter uploads those bytes wrapped in a WAV header. Neither path trims
leading silence or performs local voice activity detection.

An additional format bug was reproduced locally: the old `pw-record` invocation
emits a 24-byte AU header (`646e732e18000000ffffffff03000000803e000001000000`)
before its samples. Hyprvoice interpreted this header as PCM. At the configured
format that is only 0.75 ms of bogus samples, so this defect alone does not
explain missing whole phrases.

Five initial timing trials mistakenly measured this header as first audio:
42-55 ms including the PipeWire check. Inspecting the bytes exposed that mistake.
In subsequent trials, raw PCM first reached stdout at 202-229 ms. Each raw read
contained 4096 bytes, representing 128 ms of audio: delivery time includes
buffering, and is not a measurement of how much speech was lost. One
`pw-cli info all` probe took about 1.14 seconds. Measurements were taken on the
live desktop with other activity, not under a controlled hardware cold-start
experiment. Timing JSON is retained under the ignored `tmp/capture-startup*`
directories.

The [official file transcription guide](https://developers.openai.com/api/docs/guides/speech-to-text)
does not document a requirement for initial silence or ambient noise. That does
not prove that padding can never change recognition. No model comparison was
run here, and no saved recording containing a known missing phrase was available.
The evidence establishes capture defects, not that they explain every reported
omission. Adding silence after capture cannot restore words that were never
captured. A future failing recording must be replayed locally and compared with
its transcript to distinguish model omission from missing input.

## Fixes

- Recorder.Start waits for actual PCM and retains its first frame for the
  consumer. The notification is emitted after successful recorder and
  transcriber startup. A five-second startup timeout bounds a stalled source.
- The redundant per-recording PipeWire enumeration is removed. Process startup
  and first-sample delivery establish availability directly.
- `pw-record --raw` supplies the format downstream expects. Partial PCM samples
  are carried between reads. Unsupported recording formats are rejected in
  configuration, the editor, and the recorder rather than mislabeled as
  16000 Hz mono s16 by adapters.
- Stop sends SIGINT, reads flushed audio, and reaps the process, with a bounded
  forced-kill fallback. The previous code accumulated zombie recorder processes.
- Buffer overflow and failed streaming sends fail the session instead of
  silently producing a transcript with gaps. Pending recorder errors are checked
  before submitting audio for transcription.
- Streaming Stop drains queued audio before finalization, preserves late results
  and fatal errors, returns finalization failures, and finalizes only once.
- Batch Stop is synchronized and preserves its original result across repeated
  calls, avoiding inconsistent error reporting.
- Pipeline startup state is published synchronously. Run/Stop registration is
  synchronized, cancellation stops producers before waiting for consumers, and
  completed sessions close their event channels.
- Daemon control operations are serialized. Duplicate inject commands cannot
  block, LLM processing can be cancelled with the toggle, notification reads are
  synchronized with configuration reloads, and session monitors exit and are
  joined. IPC reads have bounded headers, context sizes, and deadlines, and close
  when the daemon shuts down.
- Tests use simulated capture processes and injection commands. Existing mock
  races were corrected so race-detector results are meaningful. Injection tests
  cannot invoke the desktop's real typing or clipboard tools.

## Validation and deployment

Regression tests cover startup readiness, first-frame preservation, PCM sample
alignment, flushed tail audio, process reaping, startup cancellation, startup EOF,
buffer overflow, streaming drain/finalize order, repeated Stop, finalization
errors, failed sends, late fatal errors, pipeline completion/cancellation,
concurrent Run/Stop, daemon monitor termination, duplicate injects, processing
cancellation, and oversized IPC context rejection.

Changes are in the repository. The running systemd service still uses its
previous Nix package; it has not been rebuilt or restarted by this investigation.
These fixes do not implement continuous microphone capture or pre-roll, so users
must still wait for the now-accurate readiness notification before speaking.

## Independent review and follow-up

The first implementation was committed as `e37357b`, then independently reviewed
by a subagent at the user's request. The review identified seven additional
problems, all addressed in the follow-up:

1. Automatic streaming reconnects could abandon earlier uncommitted audio and
   inject only a suffix. Reconnects were removed; a disconnected session now fails.
2. Finalization accepted an unrelated earlier segment's completion. OpenAI now
   disables VAD and commits manually at stop. ElevenLabs commits manually every
   20 seconds and awaits each response, then separately commits the tail.
   Deepgram waits for the terminal normal server close after `CloseStream`,
   preserving all final segments.
3. WebSocket writes could block while holding the adapter mutex, preventing
   cancellation. Established sockets now close on context cancellation, writes
   have deadlines, and finalization waits are bounded. Close no longer writes
   a competing unbounded WebSocket close message. Result delivery also responds
   to cancellation when a consumer stops reading.
4. Connection checks and subsequent writes could use different pointer values,
   including nil after a failed reconnect. Writes now use the checked connection.
5. A descendant holding the recorder's manual stdout pipe could prevent Stop
   from returning even after the parent exited. A pipe read deadline now bounds
   this case, with a regression that creates an inheriting child process.
6. A stalled `notify-send` could block the joined session monitors forever.
   Notification commands now have a two-second timeout and are stubbed in tests.
7. Errors, timeouts, and reloads could leave the persistent recording notification
   visible. Unsuccessful announced sessions now emit a terminal cancellation
   event. Abort notifications also reach the persistent replacement branch even
   when their unchanged default message definition marks them as errors.

A second review found that Deepgram's structured Error responses could still
permit partial transcript injection. Both nested and top-level error fields are
now handled; a server Error or malformed streaming response fails the session.
OpenAI commit rejection and failed transcription also terminate promptly. The
reviewer confirmed that this resolves the remaining blocker and found no further
blocking concern in the reviewed changes.

Provider behavior was checked against the official
[Deepgram CloseStream documentation](https://developers.deepgram.com/docs/close-stream),
[OpenAI realtime transcription guide](https://developers.openai.com/api/docs/guides/realtime-transcription),
and [ElevenLabs commit strategy guide](https://elevenlabs.io/docs/eleven-api/guides/how-to/speech-to-text/realtime/transcripts-and-commit-strategies).
The ElevenLabs guide explicitly notes that manual mode still automatically
commits at about 36 seconds. The 20-second acknowledged segments avoid crossing
that boundary. Waiting for segment completion can fill the recorder queue on a
slow service; the session then fails explicitly rather than dropping audio.

Final verification: 392 passing tests and two skipped tests across repository
packages excluding injection and ignored scratch packages; 16 passing injection
tests in a separate invocation. Both runs used the race detector. Build and
`go vet` passed for `cmd/hyprvoice` and `internal/...`. Structured logs are retained
in `tmp/tests_2026-10-27_03-34-53.jsonl` and
`tmp/tests_2026-10-27_03-35-00.jsonl`. Regression coverage includes normal terminal
stream closure, disconnect without replay, rejected commits, the final tail
after periodic manual commits, blocked socket cancellation, provider errors
after a partial result, malformed responses, inherited pipes, notification
timeouts, and persistent notification replacement on timeout.
