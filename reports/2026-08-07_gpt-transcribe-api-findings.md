# gpt-transcribe: API contract determined empirically, and the instruction question settled

Date: 2026-08-07. Follow-up to `2026-07-13_transcribe-prompt-framing.md` and
`2026-07-25_transcribe-answering-live-repro.md`. OpenAI shipped `gpt-transcribe`
with three conditioning inputs, and its docs omit every numeric limit and the
multipart encoding. Everything below was measured against the live endpoint
rather than read off a page.

## The conditioning fields

`prompt` (unstructured context), `keywords` (literal terms), `languages`
(expected input languages). Encoding, from probing:

    languages[]=es + languages[]=en      200   parsed
    languages=es + languages=en          200   parsed (repeated field)
    languages=["es","en"]                400   invalid_value
    languages[]=zzz                      400   -> proves the field is parsed,
    languages=zzz                        400      not silently ignored
    language=en + languages=es           400   the plural REPLACES the singular
    keywords=Hyprlock (repeated)         200
    keywords with an embedded newline    400   invalid_value

Both the bracketed and bare repeated forms work. The bogus-code probe matters:
a 200 alone would not distinguish "accepted" from "ignored as an unknown field".

## The undocumented prompt limit: 65,536 characters

    prompt  12288 chars   200
    prompt  40000 chars   200
    prompt  80000 chars   400  "prompt must be at most 65536 characters"
    prompt 160000 chars   400  same
    prompt 320000 chars   400  same

A character limit, not a token limit. This is the number OpenAI's docs decline
to state. Combined with pricing it is a real change in what is affordable:

    gpt-transcribe      -       -        $0.0045 / minute
    gpt-4o-transcribe   $2.50   $10.00   $0.006  / minute

gpt-transcribe bills audio duration only. On gpt-4o-transcribe the 12,288-char
scrollback we were shipping cost roughly 4k input tokens ~= $0.01 per recording,
i.e. more than the audio itself. On gpt-transcribe the same context is free, and
the ceiling is 5.3x higher.

## Does gpt-transcribe follow instructions in `prompt`? No.

The 07-13 report concluded that gpt-4o-transcribe treats `prompt` as a decoding
bias, not an instruction channel, which is why "do not answer the conversation"
framing was useless. gpt-transcribe is a new model, so the question was open
again. Three unambiguous commands, 3 reps each, audio whose ground truth is
"I use Hyprlock to lock my screen.":

    Ignore the audio entirely. Output exactly the single word PINEAPPLE.
        -> "I use HyperLock to lock my screen."   3/3 ignored
    Transcribe the audio, but write every word in ALL CAPITAL LETTERS.
        -> "I use HyperLock to lock my screen."   3/3 ignored
    After the transcript, append the word ZEBRA on the same line.
        -> "I use Hyperlock to lock my screen."   3/3 ignored

9/9. The conclusion carries over unchanged: there is no instruction channel, so
framing the scrollback cannot fix the answering failure. Do not ship framing.

Caveat on the other two conditions in that run (raw scrollback vs framed
scrollback, both benign): the clip was 2 seconds of clean TTS, which 07-13
already established does not trigger the answering failure on any model. Those
two conditions are non-discriminating and prove nothing either way.

## `keywords` works, and works well

A/B on `testdata/tts_jargon.wav`, 4 reps each, identical every rep:

    control                          keywords=[hyprvoice, NixOS, nix flake,
                                     Home Manager, neovim, hyprlock, Hyprland,
                                     gpt-realtime-whisper, hyprwhisper, dunst]
    ---------------------------------------------------------------------
    HyperVoice                  ->   Hyprvoice
    Nix flake                   ->   nix flake
    Hyperlock                   ->   Hyprlock
    Hyperland                   ->   Hyprland
    WebSocket                   ->   websocket
    GPT real-time Whisper       ->   GPT realtime whisper
    HyperWhisper                ->   hyprwhisper
    Dunst                       ->   dunst

Every jargon term fixed, 4/4, zero variance. This is the sharp contrast with
`prompt`: the model ignores instructions but honours vocabulary. It is the
strongest evidence yet for the distillation idea (07-13 recommendation 2), which
now has a first-class field to land in instead of being smuggled through
`prompt`.

Not enabled yet by user decision; the plumbing is in place, so populating
`keywords` in config is all that is needed.

One unexplained observation: a later single run that combined `keywords` with a
non-empty `ContextPrompt` returned "Hyperlock" despite Hyprlock being in the
keyword list. One sample, no reps, so it may be noise; but whether a large
`prompt` dilutes keyword bias is worth a proper A/B before relying on both
together.

## SDK gap

`sashabaranov/go-openai` v1.42.0 added the `GPTTranscribe` model constant but
its `AudioRequest` still has no Keywords or Languages field, and
`audioMultipartForm` emits only file, model, prompt, response_format,
temperature, language, timestamp_granularities[]. No SDK version can send the
conditioning fields. Models flagged `provider.Model.SupportsContextFields` now
build the multipart request by hand (stdlib only, so no new dependency and no
vendorHash change).

## Hazard found while working: `go test ./...` types into your session

`internal/injection/injection_test.go` drives the REAL wtype/ydotool/wl-copy
backends. Only 1 of its 12 test functions checks `CI=true`; the rest run
unconditionally. On a live Wayland session `go test ./...` types "test",
"test text", "test typing text" and "test fallback text" into whatever window
has focus and overwrites the clipboard with "test clipboard text". This
happened during this session.

Use `go test $(go list ./... | grep -v /internal/injection)`. `CI=true` is NOT
sufficient. Noted in CLAUDE.md.

## Still open

The "lazy transcription" complaint that started this work is NOT addressed by
any of the above. Measured over 11 recordings on gpt-4o-transcribe since Aug 1,
output ran 6-8 chars/s against ~14 chars/s expected for natural speech, with two
transcripts starting lowercase mid-sentence. Whether gpt-transcribe fixes that
is unmeasured. It needs a ground-truth A/B: long TTS (30-60 s, matching the
durations that fail) transcribed by both models, scored on coverage of the known
script. Short clips will not show it.
