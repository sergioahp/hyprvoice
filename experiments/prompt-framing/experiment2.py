#!/usr/bin/env python3
"""Focused follow-up: fuller context (whole codex session, ends on question) vs
hard low-confidence audio stimuli, comparing raw / dash-instr / xml-notags.

Targets the actual failure mechanism (model answers/fabricates from prompt
instead of transcribing). Reuses helpers from experiment.py.
"""
import argparse, concurrent.futures as cf, json, os, threading, time
from datetime import datetime
import experiment as E

HERE = os.path.dirname(os.path.abspath(__file__))
CTX = open(os.path.join(HERE, "context_full.txt")).read()

FRAMINGS = {
    "raw": CTX,
    "dash_instr": E.INSTR + "\n---\n" + CTX + "\n---",
    "xml_instr_notags": E.INSTR + E.NOTAGS + "\n<terminal_context>\n" + CTX + "\n</terminal_context>",
}

AUDIOS = {
    "ask_layout": ("audio/ask_layout.wav",
        "ok is it me or before the icon change very short titles were centered "
        "can you make them centered again with the icon still on the left"),
    "reply_short": ("audio/reply_short.wav", "yes do that but keep the icon on the left"),
    "trail_off": ("audio/trail_off.wav", "ok so um is it me or"),
    "ask_trunc": ("audio/ask_trunc.wav", "ok is it me or before the icon"),
    "silence": ("audio/silence.wav", ""),
    "ask_noisy": ("audio/ask_noisy.wav",
        "ok is it me or before the icon change very short titles were centered "
        "can you make them centered again with the icon still on the left"),
}

lock = threading.Lock()
state = {"spent": 0.0, "stop": False}


def run_one(framing, prompt, audio_key, rep, log_path):
    with lock:
        if state["stop"]:
            return
    rel, expected = AUDIOS[audio_key]
    path = os.path.join(HERE, rel)
    fields = {"model": E.MODEL, "prompt": prompt}
    t0 = time.time()
    try:
        resp = E.post_multipart(fields, path); err = None
    except Exception as ex:
        resp, err = {}, f"{type(ex).__name__}: {ex}"
    dt = time.time() - t0
    text = resp.get("text", "")
    cost = resp.get("usage", {}).get("cost", 0.0) or 0.0
    rec = {"ts": datetime.now().astimezone().isoformat(timespec="milliseconds"),
           "framing": framing, "audio": audio_key, "rep": rep,
           "latency_s": round(dt, 2), "cost_usd": cost, "error": err, "text": text}
    if not err:
        rec.update(E.score(text, expected))
        # silence has empty expected; treat any real words as a hallucination
        if audio_key == "silence":
            rec["halluc"] = len(E.norm(text)) > 3
    with lock:
        state["spent"] += cost
        if state["spent"] >= E.BUDGET_USD:
            state["stop"] = True
        with open(log_path, "a") as f:
            f.write(json.dumps(rec) + "\n")
        flag = "ERR " if err else ("HALLUC" if rec.get("halluc") else "ok    ")
        extra = "+tags" if rec.get("tag_leak") else ""
        bleed = len(rec.get("bleed_phrases", []))
        print(f"{framing:>17}/{audio_key:<11} r{rep} {flag}{extra} "
              f"bleed={bleed} ${state['spent']:.3f} {text[:60]!r}", flush=True)


def main():
    ap = argparse.ArgumentParser(); ap.add_argument("--reps", type=int, default=12)
    args = ap.parse_args()
    ts = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
    log_path = os.path.join(HERE, "logs", f"run2_full_reps-{args.reps}_{ts}.jsonl")
    os.makedirs(os.path.dirname(log_path), exist_ok=True)
    print(f"logging to {log_path}")
    jobs = [(fr, pr, a, r) for fr, pr in FRAMINGS.items() for a in AUDIOS for r in range(args.reps)]
    print(f"{len(jobs)} calls planned")
    with cf.ThreadPoolExecutor(max_workers=6) as ex:
        futs = [ex.submit(run_one, *j, log_path) for j in jobs]
        for f in cf.as_completed(futs):
            f.result()
    print(f"\ntotal spent: ${state['spent']:.4f}")
    print(f"log: {log_path}")


if __name__ == "__main__":
    main()
