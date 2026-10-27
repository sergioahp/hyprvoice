#!/usr/bin/env python3
"""Stage 1: detect settled live-transcript states in the screen recording.

Extracts the cropped live-transcript region at a fixed sample rate, then walks
the frames committing one screenshot per *settled* distinct state. "Settled"
means the region stopped changing for SETTLE frames (so we snapshot stable text,
not mid-render), and differs from the last committed state by more than
CHANGE_THRESH pixels (so static pauses don't re-emit duplicates).

No API calls here -- output is screenshots + a manifest to inspect/tune before
spending on OCR.
"""
import json
import subprocess
from pathlib import Path

import numpy as np
from PIL import Image

VIDEO = Path.home() / "2026-06-28 17-14-42.mp4"
CROP = "620:455:320:245"          # w:h:x:y  (full-frame region x[320..940] y[245..700])
FPS = 5                            # sample rate (0.2 s resolution)

DIFF_PX_THRESH = 25               # per-pixel grayscale abs-diff to count a pixel as "changed"
STILL_MAX = 40                    # <= this many changed px between consecutive frames => "still"
CHANGE_THRESH = 120               # > this many changed px vs last committed state => new state
SETTLE = 3                        # frames of stillness required before committing

BASE = Path(__file__).resolve().parent
FRAMES = BASE / "allframes"
STATES = BASE / "states"


def extract_frames() -> list[Path]:
    FRAMES.mkdir(exist_ok=True)
    existing = sorted(FRAMES.glob("f_*.png"))
    if existing:
        return existing
    subprocess.run(
        ["ffmpeg", "-nostdin", "-loglevel", "error", "-i", str(VIDEO),
         "-vf", f"crop={CROP},fps={FPS}", str(FRAMES / "f_%05d.png")],
        check=True,
    )
    return sorted(FRAMES.glob("f_*.png"))


def gray(p: Path) -> np.ndarray:
    return np.asarray(Image.open(p).convert("L"), dtype=np.int16)


def changed_px(a: np.ndarray, b: np.ndarray) -> int:
    return int(np.count_nonzero(np.abs(a - b) > DIFF_PX_THRESH))


def main() -> None:
    frames = extract_frames()
    n = len(frames)
    print(f"frames={n}  fps={FPS}  duration~={n/FPS:.1f}s  crop={CROP}")

    grays = [gray(p) for p in frames]

    STATES.mkdir(exist_ok=True)
    for old in STATES.glob("state_*.png"):
        old.unlink()

    states = []          # committed states
    motion = []          # frame-to-frame changed px, for diagnostics

    last_committed_gray = None
    still = 0

    for i, g in enumerate(grays):
        t = i / FPS
        m = changed_px(grays[i - 1], g) if i > 0 else 0
        motion.append(m)

        # always commit the very first frame as the initial state
        if last_committed_gray is None:
            _commit(states, frames[i], i, t, 0)
            last_committed_gray = g
            still = SETTLE  # already settled
            continue

        if m <= STILL_MAX:
            still += 1
        else:
            still = 0

        if still == SETTLE:
            d = changed_px(last_committed_gray, g)
            if d > CHANGE_THRESH:
                _commit(states, frames[i], i, t, d)
                last_committed_gray = g

    manifest = {
        "video": str(VIDEO), "crop": CROP, "fps": FPS,
        "params": {"DIFF_PX_THRESH": DIFF_PX_THRESH, "STILL_MAX": STILL_MAX,
                   "CHANGE_THRESH": CHANGE_THRESH, "SETTLE": SETTLE},
        "n_frames": n, "n_states": len(states), "states": states,
    }
    (BASE / "states_manifest.json").write_text(json.dumps(manifest, indent=2))

    mot = np.array(motion)
    print(f"\nmotion (changed px frame-to-frame): "
          f"min={mot.min()} median={int(np.median(mot))} "
          f"p90={int(np.percentile(mot,90))} max={mot.max()} "
          f"nonzero_frames={int(np.count_nonzero(mot))}/{n}")
    print(f"\ncommitted states: {len(states)}")
    print(f"{'idx':>3} {'t(s)':>7} {'Δpx vs prev state':>18}  file")
    for s in states:
        print(f"{s['idx']:>3} {s['t']:>7.1f} {s['dpx']:>18}  {Path(s['frame']).name}")


def _commit(states, frame_path, i, t, dpx):
    name = f"state_{len(states):03d}_t{t:06.2f}.png"
    dst = STATES / name
    Image.open(frame_path).save(dst)
    states.append({"idx": len(states), "frame_index": i, "t": round(t, 2),
                   "dpx": dpx, "frame": str(dst.relative_to(BASE))})


if __name__ == "__main__":
    main()
