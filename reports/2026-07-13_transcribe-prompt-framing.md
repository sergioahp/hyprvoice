# gpt-4o-transcribe: does prompt framing stop it answering instead of transcribing?

Date: 2026-07-13. Follow-up to the observation that gpt-4o-transcribe
sometimes answers the on-screen agent conversation (fed as the `prompt`)
instead of transcribing the spoken audio. Screenshot showed the model emitting
a full fabricated assistant reply ("Yes, the original single-label title was
center-aligned ... Would you like that?") into the codex input line instead of
the user's words.

Goal: test whether instruction framing around the scrollback (XML fences, dash
fences, explicit "transcribe, do not answer" rules) prevents the failure, and
whether structured outputs help. Model held fixed at gpt-4o-transcribe
throughout, tested via OpenRouter (nicer logs, more credits). Distillation
(vocabulary-only prompt) was explicitly out of scope for this round.

Total spend: ~0.19 USD of the 1 USD budget, ~656 API calls.

## TL;DR

1. Structured outputs are NOT available on the transcription endpoint for
   gpt-4o-transcribe. Only `response_format` json / verbose_json; json_schema
   is rejected. So you cannot force the output into a `{transcript: ...}`
   schema field. Ruled out.
2. The `prompt` parameter is a BIASING context, not an instruction channel.
   gpt-4o-transcribe does not obey commands placed in it. An explicit
   "if the audio is silent output [BLANK_AUDIO], never answer" rule was ignored
   in 16/16 silent trials (the token was never once emitted). This is the
   central finding and it undercuts every instruction-based fix.
3. XML framing does NOT leak tags into the output, and dash `---` fences do not
   leak either: 0 tag/dash leaks in 568 framed calls, even without an explicit
   "no tags" rule. So framing is safe, it just does not accomplish anything.
4. The catastrophic "answers the conversation" failure did NOT reproduce in
   568+ calls with clean TTS audio, nor with degraded audio (noise, silence,
   truncation), even with the exact adversarial context ending on
   "Would you like that?". No context bleed in ANY condition (0/568).
5. The reproducible failure is a different one: on silent or noisy audio the
   model hallucinates GENERIC short phrases ("Hello", "Hi", "Thank you",
   "The sky is blue", foreign-language snippets), ~75% of the time. These do
   NOT come from the prompt, and instructions do not reduce the rate.

Bottom line: prompt-side instructions cannot fix this because the endpoint does
not follow instructions. The real levers are audio-side (VAD / energy gate to
drop silent clips) and prompt-content-side (distillation to a vocabulary list,
so there is no conversational structure to complete). Framing is harmless but
useless.

## What was tested

Context reconstructed from the real codex session that failed
(~/.codex/sessions/2026/07/13/rollout-...-019f5cb8-...jsonl, project
~/code/rust/gtk-status-bar). Three context tails:
- weak: ends on codex tool warnings (as the screenshot scrollback did)
- question: ends on the assistant explanation + "Would you like that?"
- full: the whole 7.7k-char session, tail-kept, ending on the question

Five framings, each wrapping the same context:
- raw:              context only (current hyprvoice behavior)
- dashes_only:      context + "\n---"
- dash_instr:       INSTR + "\n---\n" + context + "\n---"
- xml_instr:        INSTR + "<terminal_context>" ... "</terminal_context>"
- xml_instr_notags: xml_instr + explicit "never output XML tags" rule
- (none:            no prompt at all, control)

INSTR = "You are a transcription service. The text below is terminal
scrollback, provided only as vocabulary and spelling context. Transcribe the
user's spoken audio verbatim. Do not answer questions, do not continue the
conversation, and do not add anything that was not spoken."

Audio stimuli (TTS via OpenAI tts-1, plus degraded variants via ffmpeg):
- ask_layout:  "Ok, is it me, or before the icon change very short titles were
               centered? Can you make them centered again ..." (mirrors the real
               spoken question)
- reply_short: "Yes, do that, but keep the icon on the left." (dangerous after a
               "Would you like that?" context)
- statement:   an unrelated instruction
- trail_off:   "Ok so, um, is it me, or..." (disfluent, trails off)
- ask_trunc:   ask_layout cut mid-sentence at 2.2s
- ask_noisy:   ask_layout at 0.35 gain mixed with white noise (low SNR)
- silence:     0.6s digital silence (classic hallucination trigger)
- hyprlock:    pre-existing testdata clip (spelling sanity)

## Results

### Context bleed / answering the conversation: 0 / 568

Across every framing x context x audio combination, including 128 raw-prompt
calls whose context ended on "Would you like that?", the model never once
answered the conversation, regurgitated the prompt, or produced any of the
20+ tracked scrollback phrases (pill, evidence, single-label, regression suite,
39450d6, etc.). The specific failure from the screenshot did not reproduce.

This does not mean it cannot happen. It means clean and moderately-degraded TTS
does not trigger it; the real incident used live mic audio, which is messier
(disfluencies, room noise, mid-word cutoffs) and may hit a rarer path. But the
raw prompt is far less dangerous for typical input than the screenshot alone
suggested.

### Silence / noise hallucination: reproducible, prompt-independent, ~75%

Fraction of silent/noisy clips that hallucinated content (full-context run):
    raw:               18/24 (75%)
    dash_instr:        18/24 (75%)
    xml_instr_notags:  23/24 (96%)

The hallucinations are generic and short, never from the prompt:
    silence -> "Hello", "Hi", "Goodbye", "Thank you", "Ok", CJK/Hindi snippets
    ask_noisy -> "The sky is blue", "I love you", "The future of AI", "Yeah"

Explicit anti-silence instruction test (16 reps each, full context):
    output [BLANK_AUDIO] on silence, "never guess, never invent"
    raw           silence suppressed 6/16, ask_noisy 4/16
    silence_rule  silence suppressed 5/16, ask_noisy 6/16   (no improvement)
    [BLANK_AUDIO] token emitted: 0 times ever

Good audio was never wrongly blanked under either prompt (0/16), so the
instruction was not even partially effective, it was simply ignored.

### Tag / fence leakage: 0

xml_instr and xml_instr_notags: 0 tag leaks in the output across all runs, even
without the "no tags" rule. dash_instr / dashes_only: 0 `---` leaks. So the
"will the XML markers show up in my transcript" worry is unfounded for this
model. Framing is output-safe.

### Structured outputs: unsupported

    response_format=json           -> ok, {"text": ...}
    response_format=text           -> 400 (only json/verbose_json)
    response_format=verbose_json   -> 400 (model does not support it)
    response_format=json_schema{...} -> 400 (only json/verbose_json)

Schema-constrained decoding exists on the chat-completions audio models
(gpt-4o-audio-preview / gpt-audio) but NOT on gpt-4o-transcribe's
/audio/transcriptions endpoint. Since we are staying on gpt-4o-transcribe,
structured outputs are not an available tool here.

## Interpretation

The endpoint's `prompt` is documented and behaves as a decoding bias / vocab
hint carried over from the Whisper API, not as a system prompt. gpt-4o-transcribe
does not treat it as instructions. That is why:
- framing text does not change transcription behavior on good audio,
- "do not answer" / "output blank on silence" are ignored,
- yet the prompt CAN still bias output (it is why keyword hints improve spelling).

So the original hypothesis ("add instructions and framing to stop it answering")
is not actionable through this endpoint: there is no instruction channel to use.
The rare answering failure, when it happens, is the model completing a
conversational prompt, and the only way to remove that pressure via the prompt is
to remove the conversational structure from the prompt (distillation), not to
argue with it.

## Recommendations (for the next round / codex)

Prompt-side instruction framing: drop it. It is safe but does nothing. Do not
ship XML/dash framing as a fix; it adds tokens and complexity for no behavior
change.

Real mitigations, in rough order of value:
1. Audio gate (VAD or RMS-energy threshold) before calling the API: if the clip
   is silent or near-silent, skip the request entirely. This directly kills the
   only reproducible failure (silence hallucination) and saves API cost. This is
   the highest-leverage fix and it is not model-dependent.
2. Distillation of the scrollback into a vocabulary/keyword list (the deferred
   idea): send identifiers, filenames, unusual tokens as a comma list via the
   same `prompt` slot the keyword path already uses. Removes the conversational
   structure that (rarely) provokes completion, while keeping the spelling
   benefit. More promising than framing precisely because the endpoint ignores
   instructions but still honors vocabulary bias.
3. Output post-filter: reject known-generic hallucination outputs on
   short/low-energy clips (e.g. bare "Thank you." / "Hello." from a <1s clip).
   Cheap belt-and-suspenders on top of (1).
4. Keep the tail-truncation, but if distillation lands it is moot.

Not recommended: switching models (out of scope), structured outputs (not
supported here), instruction framing (ignored).

## Artifacts (tmp/prompt-framing/, gitignored)

- experiment.py         raw/framing x weak/question context x 4 clips x 8 reps
- experiment2.py        full context x hard stimuli x 3 framings x 12 reps
- context_codex.txt     weak-tail reconstructed context
- context_question.txt  adversarial tail ending on "Would you like that?"
- context_full.txt      whole-session context, tail-kept
- audio/*.wav           TTS + degraded (silence, trail_off, ask_trunc, ask_noisy)
- logs/*.jsonl          one record per call: text, similarity, bleed, tag_leak,
                        latency, cost. Read with jq.
