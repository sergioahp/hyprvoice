// tui is an interactive controller + live transcript viewer for the realtime-lab
// prototypes. It launches a prototype as a child process, renders its JSONL event
// stream (committed + volatile draft) live, and sends commands (replay/mic/stop/
// quit) to the prototype's unix control socket on keypress.
//
// Run from the realtime-lab dir (or its tui/ subdir):
//
//	nix develop /home/admin/code/go/hyprvoice --command go run ./tui
//
// Keys: 1-5 pick idea, tab next, r replay sample, R replay test-long.wav,
//
//	m mic, s stop, c clear, l log, ? help, q quit.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- prototype event (matches INTERFACE.md JSONL on stdout) ----

type protoEvent struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	State string `json:"state"`
}

// ---- bubbletea messages (gen tags ignore stale child output after a switch) ----

type evtMsg struct {
	gen int
	e   protoEvent
}
type logMsg struct {
	gen  int
	line string
}
type sockMsg struct {
	gen  int
	path string
}
type exitMsg struct {
	gen int
	err error
}
type buildMsg struct {
	path string
	err  error
}
type ackMsg struct{ line string }
type tickMsg time.Time

// ---- model ----

type idea struct{ name, dir string }

type model struct {
	ideas []idea
	cur   int // selected idea index; -1 = none running

	gen      int
	cancel   context.CancelFunc
	sockPath string
	alive    bool
	busy     string // non-empty while building/transitioning

	binPath string

	committed, draft, status string
	done                     bool

	start      time.Time
	firstWord  time.Duration
	commits    int
	drafts     int
	lastUpdate time.Time

	logs    []string
	showLog bool
	help    bool

	w, h   int
	events chan tea.Msg
}

func newModel(ideas []idea) model {
	return model{ideas: ideas, cur: -1, events: make(chan tea.Msg, 256)}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(), listen(m.events))
}

func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func tick() tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---- child process management ----

// resolveBinCmd builds a fresh binary if `go` is on PATH (i.e. inside the nix
// devshell); otherwise falls back to an already-built prototype binary so the
// TUI also works when launched outside the devshell.
func resolveBinCmd(id idea) tea.Cmd {
	return func() tea.Msg {
		if _, err := exec.LookPath("go"); err == nil {
			c := exec.Command("go", "build", "-o", ".tui-bin", ".")
			c.Dir = id.dir
			if out, err := c.CombinedOutput(); err != nil {
				if p := findExe(id.dir); p != "" {
					return buildMsg{path: p}
				}
				return buildMsg{err: fmt.Errorf("build: %v: %s", err, strings.TrimSpace(string(out)))}
			}
			return buildMsg{path: filepath.Join(id.dir, ".tui-bin")}
		}
		if p := findExe(id.dir); p != "" {
			return buildMsg{path: p}
		}
		return buildMsg{err: fmt.Errorf("no `go` on PATH and no prebuilt binary in %s", id.dir)}
	}
}

// findExe returns a runnable prototype binary in dir (prefers .tui-bin).
func findExe(dir string) string {
	if fi, err := os.Stat(filepath.Join(dir, ".tui-bin")); err == nil && !fi.IsDir() {
		return filepath.Join(dir, ".tui-bin")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".go") || strings.HasSuffix(n, ".mod") || strings.HasSuffix(n, ".sum") ||
			strings.HasSuffix(n, ".wav") || strings.HasSuffix(n, ".raw") || strings.HasSuffix(n, ".md") ||
			strings.HasSuffix(n, ".sock") || strings.HasSuffix(n, ".log") || strings.HasSuffix(n, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0111 != 0 {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

func (m *model) spawn(idx int) {
	id := m.ideas[idx]
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.gen++
	gen := m.gen
	m.sockPath = filepath.Join(id.dir, "control.sock")

	c := exec.CommandContext(ctx, m.binPath)
	c.Dir = id.dir
	stdout, _ := c.StdoutPipe()
	stderr, _ := c.StderrPipe()
	if err := c.Start(); err != nil {
		m.events <- exitMsg{gen: gen, err: err}
		return
	}
	m.alive = true

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var e protoEvent
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				m.events <- evtMsg{gen: gen, e: e}
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, "control socket:"); i >= 0 {
				p := strings.TrimSpace(line[i+len("control socket:"):])
				if p != "" {
					m.events <- sockMsg{gen: gen, path: p}
				}
			}
			m.events <- logMsg{gen: gen, line: line}
		}
	}()
	go func() {
		err := c.Wait()
		m.events <- exitMsg{gen: gen, err: err}
	}()
}

func sendCmd(sock, cmd string) tea.Cmd {
	return func() tea.Msg {
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			return ackMsg{line: "! " + cmd + ": " + err.Error()}
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(cmd + "\n"))
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		ack := strings.TrimSpace(string(buf[:n]))
		if ack == "" {
			ack = "sent: " + cmd
		}
		return ackMsg{line: ack}
	}
}

func (m *model) resetTranscript() {
	m.committed, m.draft, m.status, m.done = "", "", "", false
	m.commits, m.drafts, m.firstWord = 0, 0, 0
	m.start = time.Now()
	m.lastUpdate = time.Time{}
}

func (m *model) addLog(s string) {
	m.logs = append(m.logs, s)
	if len(m.logs) > 300 {
		m.logs = m.logs[len(m.logs)-300:]
	}
}

// ---- update ----

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tick()

	case tea.KeyMsg:
		return m.onKey(msg)

	case buildMsg:
		if msg.err != nil {
			m.busy = ""
			m.addLog("launch failed: " + msg.err.Error())
			return m, nil
		}
		m.busy = ""
		m.binPath = msg.path
		m.resetTranscript()
		m.spawn(m.cur)
		return m, nil

	case sockMsg:
		if msg.gen == m.gen {
			m.sockPath = msg.path
		}
		return m, listen(m.events)

	case ackMsg:
		m.addLog(msg.line)
		return m, nil

	case logMsg:
		if msg.gen == m.gen {
			m.addLog(msg.line)
		}
		return m, listen(m.events)

	case exitMsg:
		if msg.gen == m.gen {
			m.alive = false
			note := "child exited"
			if msg.err != nil {
				note += ": " + msg.err.Error()
			}
			m.addLog(note)
		}
		return m, listen(m.events)

	case evtMsg:
		if msg.gen == m.gen {
			m.applyEvent(msg.e)
		}
		return m, listen(m.events)
	}
	return m, nil
}

func (m *model) applyEvent(e protoEvent) {
	now := time.Now()
	switch e.Type {
	case "committed":
		m.committed = e.Text
		m.commits++
		m.lastUpdate = now
		if m.firstWord == 0 && e.Text != "" {
			m.firstWord = now.Sub(m.start)
		}
	case "draft":
		m.draft = e.Text
		m.drafts++
		m.lastUpdate = now
		if m.firstWord == 0 && e.Text != "" {
			m.firstWord = now.Sub(m.start)
		}
	case "final":
		m.committed, m.draft, m.done = e.Text, "", true
		m.lastUpdate = now
	case "status":
		if e.State != "" {
			m.status = e.State
		} else {
			m.status = e.Text
		}
	case "reset":
		m.resetTranscript()
	case "info":
		m.addLog("· " + e.Text)
	case "error":
		m.addLog("✗ " + e.Text)
	}
}

func (m model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c":
		if m.alive && m.sockPath != "" {
			_ = func() tea.Msg { return sendCmd(m.sockPath, "quit")() }() // best-effort
		}
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit

	case "?":
		m.help = !m.help
		return m, nil
	case "l":
		m.showLog = !m.showLog
		return m, nil
	case "c":
		m.resetTranscript()
		return m, nil

	case "1", "2", "3", "4", "5":
		idx := int(k.String()[0] - '1')
		if idx < len(m.ideas) {
			return m.switchTo(idx)
		}
		return m, nil
	case "tab":
		if len(m.ideas) > 0 {
			return m.switchTo((m.cur + 1 + len(m.ideas)) % len(m.ideas))
		}
		return m, nil

	case "r":
		return m, m.send("replay")
	case "R":
		return m, m.send("replay test-long.wav")
	case "m":
		return m, m.send("mic")
	case "s":
		return m, m.send("stop")
	}
	return m, nil
}

func (m *model) send(cmd string) tea.Cmd {
	if !m.alive || m.sockPath == "" {
		m.addLog("! no prototype running (press 1-5)")
		return nil
	}
	return sendCmd(m.sockPath, cmd)
}

func (m model) switchTo(idx int) (tea.Model, tea.Cmd) {
	// gracefully stop current child
	if m.alive && m.sockPath != "" {
		go sendCmd(m.sockPath, "quit")()
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.alive = false
	m.cur = idx
	m.sockPath = ""
	m.busy = "launching " + m.ideas[idx].name + " ..."
	m.resetTranscript()
	return m, resolveBinCmd(m.ideas[idx])
}

// ---- view ----

var (
	stTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	stDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	stDraft = lipgloss.NewStyle().Foreground(lipgloss.Color("208")) // orange "head": volatile, not yet committed
	stBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	stKeys  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func statusColor(s string, done bool) lipgloss.Style {
	c := "14" // cyan
	switch {
	case done || s == "done":
		c = "10"
	case s == "recording":
		c = "10"
	case s == "transcribing", s == "processing":
		c = "11"
	case s == "" || s == "idle":
		c = "242"
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c))
}

func (m model) View() string {
	if m.w == 0 {
		return "loading..."
	}
	name := "(none)"
	if m.cur >= 0 {
		name = m.ideas[m.cur].name
	}
	st := m.status
	if m.done {
		st = "done"
	}
	if st == "" {
		st = "idle"
	}
	if m.busy != "" {
		st = m.busy
	}
	header := stTitle.Render("realtime-lab") + "  " +
		stDim.Render(name) + "  " + statusColor(m.status, m.done).Render("● "+st)

	// footer
	dl := "—"
	if !m.lastUpdate.IsZero() {
		dl = fmt.Sprintf("%dms", time.Since(m.lastUpdate).Milliseconds())
	}
	fw := "—"
	if m.firstWord > 0 {
		fw = fmt.Sprintf("%dms", m.firstWord.Milliseconds())
	}
	metrics := stDim.Render(fmt.Sprintf("t=%.1fs  first-word=%s  commits=%d  drafts=%d  Δ=%s",
		time.Since(m.start).Seconds(), fw, m.commits, m.drafts, dl))
	keys := stKeys.Render("[1-5]idea [tab]next  [r]eplay [R]long [m]ic [s]top [c]lear  [l]og [?]help [q]uit")

	chrome := 2 /*header+blank*/ + 2 /*box border*/ + 3 /*metrics+keys+pad*/
	logH := 0
	if m.showLog {
		logH = 7
	}
	bodyH := m.h - chrome - logH
	if bodyH < 3 {
		bodyH = 3
	}
	innerW := m.w - 4
	if innerW < 10 {
		innerW = 10
	}

	var body string
	if m.help {
		body = m.helpText()
	} else if m.cur < 0 {
		body = m.pickerText()
	} else {
		body = m.transcriptText(innerW, bodyH)
	}
	box := stBox.Width(innerW).Height(bodyH).Render(body)

	parts := []string{header, box, metrics, keys}
	if m.showLog {
		parts = append(parts, m.logBox(innerW))
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m model) transcriptText(w, h int) string {
	lines := wrapTranscript(m.committed, m.draft, w)
	if len(lines) == 0 {
		return stDim.Render("(waiting for audio — press r to replay, m for mic)")
	}
	if len(lines) > h { // show the tail (live, last part)
		lines = lines[len(lines)-h:]
	}
	return strings.Join(lines, "\n")
}

func (m model) pickerText() string {
	var b strings.Builder
	b.WriteString("Pick a prototype to launch:\n\n")
	for i, id := range m.ideas {
		fmt.Fprintf(&b, "  %d  %s\n", i+1, id.name)
	}
	b.WriteString("\n" + stDim.Render("press 1-5"))
	return b.String()
}

func (m model) helpText() string {
	return strings.Join([]string{
		"Keys",
		"  1-5    select / (re)launch that prototype",
		"  tab    next prototype",
		"  r      replay ../sample.wav",
		"  R      replay test-long.wav (idea-e: triggers 30s commit)",
		"  m      mic (live capture from default source)",
		"  s      stop (finalize -> final transcript)",
		"  c      clear the transcript view",
		"  l      toggle log pane (stderr / info / errors)",
		"  ?      toggle this help",
		"  q      quit",
		"",
		"committed text is solid; the volatile \"head\" (draft tail) is orange.",
	}, "\n")
}

func (m model) logBox(w int) string {
	n := 5
	start := 0
	if len(m.logs) > n {
		start = len(m.logs) - n
	}
	body := strings.Join(m.logs[start:], "\n")
	if body == "" {
		body = stDim.Render("(no log output)")
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1).
		Width(w).Height(n).Render(body)
}

// wrapTranscript word-wraps committed (normal) + draft (dim) to width w.
func wrapTranscript(committed, draft string, w int) []string {
	type tok struct {
		s     string
		draft bool
	}
	var toks []tok
	for _, x := range strings.Fields(committed) {
		toks = append(toks, tok{x, false})
	}
	for _, x := range strings.Fields(draft) {
		toks = append(toks, tok{x, true})
	}
	if len(toks) == 0 {
		return nil
	}
	var lines []string
	var cur strings.Builder
	curLen := 0
	for _, t := range toks {
		wl := len([]rune(t.s))
		add := wl
		if curLen > 0 {
			add++
		}
		if curLen > 0 && curLen+add > w {
			lines = append(lines, cur.String())
			cur.Reset()
			curLen = 0
			add = wl
		}
		if curLen > 0 {
			cur.WriteByte(' ')
		}
		if t.draft {
			cur.WriteString(stDraft.Render(t.s))
		} else {
			cur.WriteString(t.s)
		}
		curLen += add
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// ---- idea discovery ----

func findIdeas() (string, []idea) {
	roots := []string{".", "..", "../..", "realtime-lab"}
	for _, r := range roots {
		matches, _ := filepath.Glob(filepath.Join(r, "idea-*"))
		var ids []idea
		for _, mn := range matches {
			fi, err := os.Stat(mn)
			if err != nil || !fi.IsDir() {
				continue
			}
			abs, _ := filepath.Abs(mn)
			ids = append(ids, idea{name: filepath.Base(mn), dir: abs})
		}
		if len(ids) > 0 {
			sort.Slice(ids, func(i, j int) bool { return ids[i].name < ids[j].name })
			abs, _ := filepath.Abs(r)
			return abs, ids
		}
	}
	return "", nil
}

func main() {
	selftest := flag.String("selftest", "", "headless: run an idea (name substring), replay, print events, exit")
	flag.Parse()

	_, ideas := findIdeas()
	if len(ideas) == 0 {
		fmt.Fprintln(os.Stderr, "no idea-* dirs found; run from realtime-lab/")
		os.Exit(1)
	}

	if *selftest != "" {
		runSelftest(ideas, *selftest)
		return
	}

	p := tea.NewProgram(newModel(ideas), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		os.Exit(1)
	}
}

// runSelftest exercises the controller plumbing without a TTY.
func runSelftest(ideas []idea, want string) {
	var id idea
	for _, x := range ideas {
		if strings.Contains(x.name, want) {
			id = x
			break
		}
	}
	if id.name == "" {
		fmt.Fprintln(os.Stderr, "selftest: no idea matches", want)
		os.Exit(1)
	}
	fmt.Println("== selftest:", id.name, "==")
	bin := ""
	if _, err := exec.LookPath("go"); err == nil {
		b := exec.Command("go", "build", "-o", ".tui-bin", ".")
		b.Dir = id.dir
		if out, err := b.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build: %v\n%s\n", err, out)
			os.Exit(1)
		}
		bin = filepath.Join(id.dir, ".tui-bin")
	} else {
		bin = findExe(id.dir)
	}
	if bin == "" {
		fmt.Fprintln(os.Stderr, "no binary to run")
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := exec.CommandContext(ctx, bin)
	c.Dir = id.dir
	stdout, _ := c.StdoutPipe()
	stderr, _ := c.StderrPipe()
	_ = c.Start()
	sock := filepath.Join(id.dir, "control.sock")
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			fmt.Fprintln(os.Stderr, "[err]", sc.Text())
		}
	}()
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var lastDraft string
		for sc.Scan() {
			var e protoEvent
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			switch e.Type {
			case "committed":
				fmt.Println("[committed]", e.Text)
			case "final":
				fmt.Println("[FINAL]", e.Text)
			case "draft":
				lastDraft = e.Text
			case "status":
				fmt.Println("[status]", e.State)
			}
			_ = lastDraft
		}
	}()
	// wait for socket
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	one := func(cmd string) {
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dial:", err)
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(cmd + "\n"))
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		fmt.Println(">>", cmd, "=>", strings.TrimSpace(string(buf[:n])))
	}
	one("replay")
	time.Sleep(20 * time.Second)
	one("stop")
	time.Sleep(5 * time.Second)
	one("quit")
	time.Sleep(500 * time.Millisecond)
	cancel()
}
