# gpt-4o-transcribe answered the scrollback instead of transcribing: live repro

Date: 2026-07-25. Direct follow-up to
`2026-07-13_transcribe-prompt-framing.md`, whose central negative result was
that the "model answers the on-screen conversation" failure did NOT reproduce
in 568+ synthetic calls. It reproduced today, twice in a row, on live mic
audio. This report records the evidence before journald rotates it.

Raw journal excerpt saved to `tmp/2026-07-25_transcribe-answering-repro/`
(gitignored).

## What the user saw

Typed a question into a codex prompt box in kitty, then triggered hyprvoice
with context. Instead of their speech appearing, a long markdown essay
answering that typed question was typed into the terminal, truncated mid-word,
followed by a "Hyprvoice Error: Failed to inject text: all injection backends
failed, last error: wtype failed: signal: killed" notification.

Three separate defects stacked into that one symptom.

## Defect 1: the transcription endpoint answered the prompt

Two consecutive recordings, both with `ContextPrompt` = raw terminal
scrollback (tail-kept to 12,288 chars, `internal/transcriber/adapter_openai.go:81`).

    14:30:26  1,334,580 bytes audio (41.7 s of real speech)  -> 5.06 s API call
    14:31:53    915,422 bytes audio (28.6 s of real speech)  -> 5.49 s API call

Neither output is a transcript. Both are fully-formed assistant answers to the
question the user had TYPED into the codex input box, which was the tail of the
scrollback fed as `prompt`. Excerpt of call 2:

> In the context of the Chinchilla dataset, the removal of eight points during
> preprocessing raised concerns about the sensitivity of the results to such
> changes. Here are some arguments to counter the claim that the eight-point
> drop implies high sensitivity:
>
> 1. **Outlier Influence**: The removed points could have been significant
>    outliers. ...

Six numbered points, markdown bold, a closing "By emphasizing these points, you
can argue that ...". Call 1 is the same shape ("One way to argue that the
8-point drop doesn't imply ... In conclusion, there are a number of ways to
argue ...").

### Why this contradicts the 07-13 result, and what is new

The 07-13 experiment used short TTS clips (2-8 s) and adversarial context tails
ending on an assistant question ("Would you like that?"). 0/568 bleed. The
conditions that differ here, in rough order of suspicion:

1. **Long real speech (29-42 s)** vs short clean TTS. Every 07-13 stimulus was
   an order of magnitude shorter. A long, disfluent, room-noise clip gives the
   decoder far more opportunity to drift off the audio, and the prompt is the
   only other attractor.
2. **The context tail is an unanswered question sitting in an input box**, not
   an assistant question awaiting a yes/no. The natural continuation of
   "user asks X, cursor blinking" is the answer to X. The 07-13 "question" tail
   put the model in the user's seat (reply to the assistant); this one puts it
   in the assistant's seat (answer the user). That is a much stronger
   completion pressure and it was never tested.
3. **Self-amplification**: call 1's partial output was typed into the terminal,
   so call 2's scrollback contained an essay-in-progress on the same topic,
   making "continue writing the essay" even more likely. Both calls failed, so
   this is an amplifier, not the trigger.

Hypothesis (2) is the cheap one to test and would explain both the 07-13 null
and today's repro: **it is not "context ends in a question", it is "context
ends in a question addressed to an assistant, in an input field"**.

### Bearing on the 07-13 recommendations

This strengthens recommendation 2 (distill scrollback to a vocabulary/keyword
list) and weakens nothing else. Instruction framing remains useless because the
endpoint has no instruction channel; the only lever is removing the
conversational structure from the prompt. Today's failure is exactly the
completion-pressure mechanism that distillation removes, and it cost the user a
submitted prompt and a corrupted terminal buffer, so it is no longer a
theoretical tail risk.

Audio gating (recommendation 1) would NOT have helped here: 42 s of real speech
passes any VAD.

## Defect 2: wtype was SIGKILLed mid-injection, leaving partial text

`injection.wtype_timeout` is 5 s (config default, and the user's config).
`wtypeBackend.Inject` uses `exec.CommandContext`, so timeout means SIGKILL to a
process that is halfway through typing.

    14:31:53  transcription completed (~2,100 chars)
    14:31:58  Injection: wtype failed: signal: killed

The screen shows the injected text stopping mid-word at
"3. **Consistency in Tren", roughly 1,100 chars in, i.e. wtype types about
220 chars/s and would have needed ~10 s. The 5 s timeout is not "the injection
failed", it is "half your text is in the buffer and there is no undo".

The timeout must scale with text length (a base plus per-character allowance),
because transcriptions are unbounded in size while the timeout is a constant.

## Defect 3: the config's invalid backend has silently disabled the fallback

`~/.config/hyprvoice/config.toml` has

    backends = ["wl-clipboard", "wtype"]

`wl-clipboard` is not a valid backend name (valid: `ydotool`, `wtype`,
`clipboard`). Consequences, all visible in the journal:

- `Injection: unknown backend "wl-clipboard", skipping` on every injection, so
  the chain is wtype-only and there is no clipboard fallback when wtype dies.
- At startup the daemon logs a *validation warning* and continues
  (`Config manager: validation warning: invalid injection.backends ...`).
- On any subsequent edit, hot reload REJECTS the whole file
  (`Config manager: invalid config after reload: ...`), so config changes have
  been silently not applying for as long as this typo has been there.

This is the same typo flagged in the 2026-07-06 incident notes; it was never
fixed.

Note the ordering also needs thought: the `clipboard` backend only runs
`wl-copy`, it does not paste. Putting it first would mean text is never typed
at all, it would only ever land on the clipboard. Correct order is
`["wtype", "clipboard"]` so the clipboard is a genuine fallback.

## Recommended fixes

1. Distill the scrollback into a vocabulary list before putting it in `prompt`
   (07-13 recommendation 2). Highest value, directly targets defect 1.
2. Scale the wtype timeout with text length instead of a flat 5 s, and log the
   number of characters actually at risk when a kill happens.
3. Fix `backends` to `["wtype", "clipboard"]`, and consider making an invalid
   backend name a hard startup error rather than a warning, since the current
   behaviour is a silently degraded chain plus a permanently wedged hot reload.
4. Cheap experiment to confirm the mechanism in defect 1: rerun the 07-13
   harness with (a) 30-45 s stimuli and (b) a context tail that is a user
   question sitting in an input box rather than an assistant question. If bleed
   appears there and not in the old conditions, hypothesis (2) is confirmed.
