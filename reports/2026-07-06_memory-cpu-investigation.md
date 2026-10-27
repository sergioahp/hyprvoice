# Investigation: 17G "Hyprland" memory peak, intermittent 100% CPU, and related findings

Date: 2026-07-06. Follow-up to the same-day incident where hyprvoice's wtype
injection triggered the SUPER+Q exit keybind and tore down the session (looked
like a Hyprland crash; it was not - no coredump/crash report, `uwsm stop` ran
twice from Hyprland's cgroup during injection).

## 1. The 17G memory peak is not Hyprland - it is cgroup misattribution

`wayland-wm@Hyprland.service: ... 17G memory peak` at session stop counts the
whole cgroup, and your GUI apps live inside it:

- Live evidence (today, ~50 min into the fresh session): cgroup
  memory.current = 11.4G, while the Hyprland process itself has RSS 126MB and
  ~2% CPU. The cgroup contains 69 processes: Firefox + ~50 content processes
  (~9-10G RSS combined), 5+ kitty instances, Xwayland, etc.
- Why: apps are launched via `hyprctl dispatch exec` (from xremap chords and
  rofi's run-command). `dispatch exec` makes Hyprland fork the app, so it
  stays in the compositor's cgroup. The xremap config wraps the launch as
  `uwsm-app -- hyprctl dispatch exec -- <app>`, which puts only the
  short-lived `hyprctl` client in a uwsm app scope - the app itself is still
  spawned by Hyprland. Same for the historical sessions:
  - Jun 23: 16.8G peak over 26h (8h CPU total for the cgroup)
  - Jun 29: 18.6G peak + 8.1G swap peak over 5 days
  - Jul 6: 17G peak, 5h43min CPU over 9h52min (~58% of one core average)
  All of that CPU/memory is dominated by apps, not the compositor.
- Fix: launch apps as `uwsm-app -- <app>` directly (drop the
  `hyprctl dispatch exec` indirection) so each app gets its own scope in
  app-graphical.slice. Caveat: uwsm-app needs WAYLAND_DISPLAY etc. in
  xremap.service's environment; verify before switching everything.

Conclusion: no evidence of a compositor memory leak. Hyprland's own RSS is
healthy. The `Consumed ...` numbers in the journal will stay misleading until
app launching moves out of the compositor cgroup.

## 2. Intermittent 100%-of-one-core CPU: not reproducible today, sampler deployed

Code inspection of hyprvoice (branch feature/context-transcription, b3403f8)
found no busy-wait: all long-lived loops block on channels or blocking reads
(daemon accept loop, config fsnotify watcher, pipeline action loop, recording
stdout.Read, streaming/realtime adapters with bounded reconnect + backoff).
The `for { select { ... default: } }` patterns in the websocket read loops
fall through to a blocking ReadMessage, so they do not spin.

Since the incident is intermittent and not active now, a diagnostics sampler
is running to catch it live and name the culprit process:

- Script: `tmp/diag/sampler.py` (this repo, gitignored dir)
- Unit: transient user unit `hyprvoice-diag-sampler`
  (`systemctl --user status hyprvoice-diag-sampler`; it dies at logout -
  restart with:
  `systemd-run --user --unit=hyprvoice-diag-sampler --property=Nice=10 ~/.nix-profile/bin/python3 ~/code/go/hyprvoice/tmp/diag/sampler.py`)
- Output: `tmp/diag/logs/sampler_interval-30s_started-<ts>.jsonl`, one record
  per 30s: cgroup mem/peak/swap/CPU%% for wayland-wm@Hyprland, hyprvoice,
  xremap; per-process stats for Hyprland/hyprvoice/xremap/Xwayland; top-5
  CPU consumers system-wide over each window; zombie counts. Any process
  above 90% of a core gets a per-thread breakdown dumped into the record.
- Reading it: `jq 'select(.top_cpu[0].cpu_pct > 80)' <file>` finds incidents.
- If hyprvoice itself is the spinner: `kill -QUIT <pid>` makes the Go runtime
  dump all goroutine stacks to stderr (lands in the journal), which pinpoints
  the loop. SIGQUIT terminates the daemon; systemd will not auto-restart it,
  so restart with `systemctl --user restart hyprvoice`.

## 3. Bug found and fixed: pw-record zombie per recording

The recorder (internal/recording/recording.go) starts pw-record but never
calls cmd.Wait(), so every recording cycle leaves a defunct pw-record owned
by the daemon. Today's live daemon had 6 zombies after 6 dictations. Zombies
cost no CPU/RAM but accumulate in the PID table for the daemon's lifetime.

- Repro (isolated daemon, fake XDG dirs, dummy API key): 3 toggle cycles ->
  3 defunct children on b3403f8; 0 with the fix.
- Fix: worktree `/home/admin/code/go/hyprvoice-worktree-zombiefix`, branch
  `fix/reap-pw-record-zombies` (commit a62eaab), one commit on top of
  feature/context-transcription: reap in captureLoop's defer.
  `go build ./...` and recording+pipeline tests pass. Merge when convenient.

## 4. Upstream status check (LeonardoTrapani/hyprvoice, checked 2026-07-06)

Decision: do not open issues or PRs upstream for now; findings below are for
our own tracking (and for whichever agent picks this up next).

- Zombie bug is NOT known and NOT patched upstream. Their current main has
  the identical internal/recording/recording.go: pw-record started via
  exec.CommandContext, no cmd.Wait() anywhere (the only Wait in the file is
  the sync.WaitGroup). Searching their issues for zombie/defunct/reap returns
  nothing; none of their PRs (open or merged) touch process reaping. The bug
  is inherited from upstream, not fork-introduced. Our fix lives only on
  fix/reap-pw-record-zombies (a62eaab).
- Upstream half-knows the keybind-collision class from the morning incident.
  Closed issue #27 ("keyboard input gets stuck"): a user reported injection
  "sometimes leads to invoking of many hotkeys of mine!" - same failure mode
  that killed our session. Maintainer could not reproduce, closed for
  inactivity, but merged PR #28 documenting that toggle keybinds should use
  Hyprland's `bindr` (fire on key RELEASE) instead of `bind`, so modifiers
  are not held when injection lands. Corroborates the held-SUPER theory.
  Our toggle goes through xremap (super-dot chord), not Hyprland binds, so
  the bindr advice does not apply directly - the equivalent mitigation is
  making the xremap chord fire on release / not leaving super held.
- Merged upstream PR #25 "sanitize line-terminating characters during
  injection" (protects against injected newlines executing commands in
  terminals). Check whether our fork already has it; if not, pull it on the
  next upstream sync.

## 5. Handoff state (Claude session ending, codex may continue)

- Zombie fix: committed, NOT merged. Worktree
  /home/admin/code/go/hyprvoice-worktree-zombiefix, branch
  fix/reap-pw-record-zombies, commit a62eaab (one commit on top of
  feature/context-transcription b3403f8). Build + recording/pipeline tests
  pass. User prefers merge (not rebase/squash) into the working branch.
- Main working tree at /home/admin/code/go/hyprvoice: untouched by this
  investigation except gitignored tmp/diag/ and this reports/ file.
  Pre-existing dirt (modified .gitignore, untracked test files) is the
  user's, do not clean it up.
- Sampler: transient user unit hyprvoice-diag-sampler running
  tmp/diag/sampler.py, logging to tmp/diag/logs/*.jsonl every 30s. Dies at
  logout; restart command is in section 2. Purpose: attribute the next
  100%-CPU / memory incident.
- Pending user decisions (section 6 below): merge the fix, config.toml
  backend rename, exit bind change, xremap uwsm-app launch pattern.

## 6. Recommendations recap (from both investigations)

1. Merge fix/reap-pw-record-zombies (zombie leak).
2. config.toml: `backends = ["wl-clipboard", "wtype"]` - "wl-clipboard" is
   invalid (valid: ydotool, wtype, clipboard), so it is skipped with a warning
   on every injection and wtype does all the work. Rename to "clipboard"
   (and consider putting it first: paste injection cannot collide with
   keybinds, which is what killed the session this morning).
3. Move the session-exit bind off plain SUPER+Q (e.g. SUPER+SHIFT+Q), since
   injected virtual-keyboard text plus a held SUPER can reach it.
4. Switch xremap/rofi app launches from `uwsm-app -- hyprctl dispatch exec --
   <app>` to `uwsm-app -- <app>` for correct systemd accounting (and saner
   OOM behavior).
5. Test artifacts live in tmp/diag/ (gitignored): isolated-daemon configs,
   patched/unpatched binaries, logs. Safe to trash the binaries anytime.
