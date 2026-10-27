# Preserved experiments and diagnostics

This directory preserves research previously kept under ignored `tmp/` paths.
The original local files remain in place. Historical reports and transcripts
refer to those old paths; the corresponding portable copies are indexed below.
These are research snapshots, separate from the production daemon.

| Directory | Contents |
| --- | --- |
| `prompt-framing/` | July 13 prompt experiments, context fixtures, synthetic audio, and per-call results |
| `prompt-instructions/` | August instruction-following probe and its saved prompt |
| `verify-adapter/` | Direct gpt-transcribe adapter probe and PCM fixture |
| `mic-diag/` | June microphone comparison recordings, retaining their device/format/timestamp names |
| `voxtral-inspect/` | Transcript-state detector, OCR script, 22 selected state images, manifest, and OCR results |
| `diagnostics/` | CPU/memory sampler, historical diagnostic logs, and a dense Go test runner |

The large sampler JSONL is preserved as gzip; use `gzip -dc` to read it. Compiled
binaries, Python caches, and the full set of intermediate video frames remain
local generated files. The selected transcript states are sufficient to inspect
the saved OCR results. Extracting the complete frame sequence again requires the
original video named in the manifest, which was outside this repository.

The five realtime transcription prototypes, their TUI, module locks, samples,
and historical output logs are preserved separately in [realtime-lab](../realtime-lab/).
From the repository root, launch the controller with:

```sh
realtime-lab/tui.sh
```

Each prototype is an independent Go module. Build it from its own directory
using the repository dev shell. API keys come from the environment; running the
prototypes or the API probes makes requests to their configured providers.

The root `test_transcribe_context.py` uses the root `claude-*.txt` context
fixtures and `testdata/tts_*.wav` samples. Its Python environment is available via
`nix develop .#python`. The older OCR scripts additionally require Pillow and
requests; no global Python installation is needed.

The adapter probe can be run from the repository root with:

```sh
nix develop --command go run ./experiments/verify-adapter
```

Pass a PCM path as the first argument to use another clip. This probe sends
audio to OpenAI. To run ordinary Go tests with dense console output and full
JSONL logs under `tmp/`, use:

```sh
nix develop --command python3 experiments/diagnostics/run-tests.py -race ./internal/recording
```

Historical journal evidence is under
[`reports/artifacts/`](../reports/artifacts/), including the July 25 live
transcription failure and the October 27 recording timing and final test logs.
The `vendor/` directory is generated: all 872 files were verified identical to
`go mod vendor` using the committed `go.mod` and `go.sum`. It is intentionally
ignored, as are the machine-specific Nix `result` link and Python bytecode.
