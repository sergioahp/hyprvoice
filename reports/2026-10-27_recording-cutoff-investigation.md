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
