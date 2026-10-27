#!/usr/bin/env python3
"""Stage 2: OCR each detected transcript state into a timeline.

Sends every saved state screenshot to a vision model and asks for the verbatim
visible text plus the orange/uncommitted tail (Mistral's live realtime view
renders the not-yet-committed hypothesis in orange). Writes:
  - transcripts.jsonl : one {idx,t,text,orange_tail} per state
  - transcripts.md    : human-readable timeline

Usage:
  python3 ocr_states.py [--provider openai|mistral|openrouter] [--model M] [--limit N]
"""
import argparse
import base64
import json
import os
import sys
import time
from pathlib import Path

import requests

BASE = Path(__file__).resolve().parent
MANIFEST = BASE / "states_manifest.json"

PROVIDERS = {
    "openai":     ("https://api.openai.com/v1/chat/completions",     "OPENAI_API_KEY",     "gpt-4o"),
    "mistral":    ("https://api.mistral.ai/v1/chat/completions",     "MISTRAL_API_KEY",    "pixtral-large-latest"),
    "openrouter": ("https://openrouter.ai/api/v1/chat/completions",  "OPENROUTER_API_KEY", "google/gemini-2.0-flash-001"),
}

PROMPT = (
    "This is a screenshot of a live speech-to-text transcript panel. "
    "Transcribe the visible text EXACTLY and verbatim: preserve punctuation, "
    "casing, and any spelling/recognition errors. Do NOT correct anything. "
    "Committed text is dark/black; a trailing portion may be rendered in "
    "orange/red, which is the model's not-yet-committed live hypothesis. "
    "Return STRICT JSON with exactly these keys: "
    '{"text": "<the full visible transcript, verbatim>", '
    '"orange_tail": "<only the substring shown in orange/red, or empty string>"}. '
    "If the panel only shows a placeholder such as 'Your live transcription "
    "will appear here', set text to an empty string."
)


def data_url(p: Path) -> str:
    return "data:image/png;base64," + base64.b64encode(p.read_bytes()).decode()


def call(provider: str, model: str, key: str, img: Path) -> dict:
    url, _, _ = PROVIDERS[provider]
    headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
    if provider == "openrouter":
        headers["HTTP-Referer"] = "https://localhost/voxtral-inspect"
    body = {
        "model": model,
        "temperature": 0,
        "response_format": {"type": "json_object"},
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": PROMPT},
            {"type": "image_url", "image_url": {"url": data_url(img)}},
        ]}],
    }
    last = None
    for attempt in range(5):
        try:
            r = requests.post(url, headers=headers, json=body, timeout=120)
            if r.status_code == 200:
                content = r.json()["choices"][0]["message"].get("content")
                if not content:                       # transient empty/None content
                    last = "empty content"
                else:
                    return json.loads(content)
            else:
                last = f"HTTP {r.status_code}: {r.text[:300]}"
        except Exception as e:  # noqa: BLE001
            last = repr(e)
        time.sleep(2 * (attempt + 1))
    raise RuntimeError(f"API failed after retries: {last}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--provider", default="openai", choices=list(PROVIDERS))
    ap.add_argument("--model", default=None)
    ap.add_argument("--limit", type=int, default=0, help="only first N states (0 = all)")
    args = ap.parse_args()

    url, env, default_model = PROVIDERS[args.provider]
    model = args.model or default_model
    key = os.environ.get(env)
    if not key:
        sys.exit(f"missing {env} in environment")

    manifest = json.loads(MANIFEST.read_text())
    states = manifest["states"]
    if args.limit:
        states = states[: args.limit]

    print(f"provider={args.provider} model={model} states={len(states)}")
    out_jsonl = BASE / "transcripts.jsonl"
    rows = []
    with out_jsonl.open("w") as fh:
        for s in states:
            t0 = time.time()
            try:
                res = call(args.provider, model, key, BASE / s["frame"])
                text = (res.get("text") or "").strip()
                tail = (res.get("orange_tail") or "").strip()
                err = None
            except Exception as e:  # noqa: BLE001 -- keep the timeline going
                text, tail, err = "", "", str(e)
            row = {"idx": s["idx"], "t": s["t"], "text": text, "orange_tail": tail}
            if err:
                row["error"] = err
            rows.append(row)
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")
            fh.flush()
            status = f"ERROR {err[:50]}" if err else f"chars={len(text):>4} tail={tail[:40]!r}"
            print(f"[{s['idx']:>2}] t={s['t']:>6.1f}s {time.time()-t0:4.1f}s {status}")

    md = ["# Live transcript timeline", "",
          f"video: {manifest['video']}", f"crop: {manifest['crop']}",
          f"provider/model: {args.provider} / {model}", ""]
    for r in rows:
        md.append(f"## t = {r['t']:.1f}s  (state {r['idx']})")
        md.append("")
        md.append(r["text"] or "_(empty / placeholder)_")
        if r["orange_tail"]:
            md.append("")
            md.append(f"> uncommitted (orange): {r['orange_tail']}")
        md.append("")
    (BASE / "transcripts.md").write_text("\n".join(md))
    print(f"\nwrote {out_jsonl.name} and transcripts.md")


if __name__ == "__main__":
    main()
