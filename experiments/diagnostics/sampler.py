#!/usr/bin/env python3
"""CPU/memory diagnostics sampler for the Hyprland session and hyprvoice.

Logs one JSON line per interval to a timestamped file in tmp/diag/logs/.
Goal: attribute the next "100% of one core" or multi-GB memory incident to a
concrete process, and track how the wayland-wm@Hyprland.service cgroup grows
(apps launched via `hyprctl dispatch exec` are accounted there, not Hyprland
itself).

Run under systemd:
  systemd-run --user --unit=hyprvoice-diag-sampler \
    /run/current-system/sw/bin/python3 /home/admin/code/go/hyprvoice/tmp/diag/sampler.py
Stop: systemctl --user stop hyprvoice-diag-sampler
"""

import glob
import json
import os
import time
from datetime import datetime

INTERVAL_S = 30
HIGH_CPU_PCT = 90.0  # per-process threshold that triggers thread detail dump
TOP_N = 5
CLK_TCK = os.sysconf("SC_CLK_TCK")
PAGE_SIZE = os.sysconf("SC_PAGE_SIZE")

USER_CG = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service"
CG_UNITS = ["wayland-wm@Hyprland.service", "hyprvoice.service", "xremap.service"]
# processes always reported individually (matched against comm or cmdline)
NAMED = ["Hyprland", "hyprvoice", "xremap", "Xwayland"]

LOG_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "logs")


def now_iso():
    return datetime.now().astimezone().isoformat(timespec="milliseconds")


def read_first(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return None


def find_cgroup(unit):
    hits = glob.glob(f"{USER_CG}/*/{unit}") + glob.glob(f"{USER_CG}/{unit}")
    return hits[0] if hits else None


def cgroup_stats(unit):
    cg = find_cgroup(unit)
    if not cg:
        return None
    out = {"path": cg.removeprefix(USER_CG + "/")}
    for f, key in [("memory.current", "mem_bytes"), ("memory.peak", "mem_peak_bytes"),
                   ("memory.swap.current", "swap_bytes")]:
        v = read_first(os.path.join(cg, f))
        if v is not None and v.isdigit():
            out[key] = int(v)
    cpu = read_first(os.path.join(cg, "cpu.stat"))
    if cpu:
        for line in cpu.splitlines():
            k, _, v = line.partition(" ")
            if k == "usage_usec":
                out["cpu_usage_usec"] = int(v)
    pids = read_first(os.path.join(cg, "cgroup.procs"))
    out["n_procs"] = len(pids.splitlines()) if pids else 0
    return out


def scan_procs():
    """Return {pid: (comm, total_ticks, rss_bytes, cmdline_head, n_zombie_children)}"""
    procs = {}
    zombies_by_ppid = {}
    for pid_dir in glob.glob("/proc/[0-9]*"):
        pid = int(os.path.basename(pid_dir))
        try:
            with open(f"{pid_dir}/stat") as f:
                stat = f.read()
            # comm may contain spaces/parens; split around them
            lparen, rparen = stat.index("("), stat.rindex(")")
            comm = stat[lparen + 1:rparen]
            fields = stat[rparen + 2:].split()
            state, ppid = fields[0], int(fields[1])
            utime, stime = int(fields[11]), int(fields[12])
            rss_pages = int(fields[21])
            if state == "Z":
                zombies_by_ppid[ppid] = zombies_by_ppid.get(ppid, 0) + 1
                continue
            cmdline = read_first(f"{pid_dir}/cmdline") or ""
            cmdline = cmdline.replace("\x00", " ").strip()[:120]
            procs[pid] = (comm, utime + stime, rss_pages * PAGE_SIZE, cmdline)
        except (OSError, ValueError):
            continue
    return procs, zombies_by_ppid


def thread_detail(pid, window_s):
    threads = []
    for tdir in glob.glob(f"/proc/{pid}/task/[0-9]*"):
        try:
            with open(f"{tdir}/stat") as f:
                stat = f.read()
            lparen, rparen = stat.index("("), stat.rindex(")")
            name = stat[lparen + 1:rparen]
            fields = stat[rparen + 2:].split()
            ticks = int(fields[11]) + int(fields[12])
            threads.append({"tid": int(os.path.basename(tdir)), "name": name,
                            "cum_cpu_s": round(ticks / CLK_TCK, 1)})
        except (OSError, ValueError):
            continue
    threads.sort(key=lambda t: -t["cum_cpu_s"])
    return threads[:8]


def main():
    os.makedirs(LOG_DIR, exist_ok=True)
    started = datetime.now().strftime("%Y-%m-%d_%H-%M-%S")
    log_path = os.path.join(LOG_DIR, f"sampler_interval-{INTERVAL_S}s_started-{started}.jsonl")
    print(f"sampler: writing to {log_path}", flush=True)

    prev, _ = scan_procs()
    prev_cg = {u: (cgroup_stats(u) or {}).get("cpu_usage_usec") for u in CG_UNITS}
    prev_t = time.monotonic()

    while True:
        time.sleep(INTERVAL_S)
        cur, zombies = scan_procs()
        t = time.monotonic()
        window = t - prev_t

        deltas = []
        for pid, (comm, ticks, rss, cmdline) in cur.items():
            if pid in prev:
                d = ticks - prev[pid][1]
                if d > 0:
                    deltas.append((d, pid, comm, rss, cmdline))
        deltas.sort(reverse=True)

        def pct(dticks):
            return round(dticks / CLK_TCK / window * 100, 1)

        top = [{"pid": p, "comm": c, "cpu_pct": pct(d), "rss_mb": round(r / 2**20, 1),
                "cmd": cl} for d, p, c, r, cl in deltas[:TOP_N]]

        named = []
        for pid, (comm, ticks, rss, cmdline) in cur.items():
            if any(n.lower() in comm.lower() or n.lower() in cmdline.lower() for n in NAMED):
                d = ticks - prev[pid][1] if pid in prev else 0
                entry = {"pid": pid, "comm": comm, "cpu_pct": pct(d),
                         "rss_mb": round(rss / 2**20, 1),
                         "zombie_children": zombies.get(pid, 0)}
                if pct(d) >= HIGH_CPU_PCT:
                    entry["threads"] = thread_detail(pid, window)
                named.append(entry)

        cgs = {}
        for u in CG_UNITS:
            s = cgroup_stats(u)
            if s:
                usec = s.get("cpu_usage_usec")
                if usec is not None and prev_cg.get(u) is not None:
                    s["cpu_pct"] = round((usec - prev_cg[u]) / 1e6 / window * 100, 1)
                prev_cg[u] = usec
                cgs[u] = s

        hot = [e for e in top if e["cpu_pct"] >= HIGH_CPU_PCT]
        for e in hot:
            e["threads"] = thread_detail(e["pid"], window)

        rec = {
            "ts": now_iso(),
            "window_s": round(window, 2),
            "loadavg": read_first("/proc/loadavg"),
            "cgroups": cgs,
            "named": sorted(named, key=lambda e: -e["cpu_pct"]),
            "top_cpu": top,
            "total_zombies": sum(zombies.values()),
        }
        with open(log_path, "a") as f:
            f.write(json.dumps(rec) + "\n")

        prev, prev_t = cur, t


if __name__ == "__main__":
    main()
