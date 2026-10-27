// visualizer renders a live, revisable transcript stream on the terminal.
//
// It reads newline-delimited JSON events (JSONL) from stdin (or a unix socket
// with --connect) and re-renders the whole transcript block in place on every
// event, so words can change "backwards" as a stronger model corrects an
// earlier draft. It is intentionally simple, not pretty.
//
// Event shapes (one JSON object per line; unknown fields ignored):
//
//	{"type":"committed","text":"<FULL authoritative transcript so far>"}
//	{"type":"draft","text":"<FULL volatile tail being refined>"}
//	{"type":"final","text":"<FULL final transcript>"}        // marks done
//	{"type":"status","state":"recording|transcribing|idle","text":"..."}
//	{"type":"info","text":"..."}                              // scrolls above block
//	{"type":"error","text":"..."}                             // scrolls above block
//	{"type":"reset"}                                          // clear for new utterance
//
// committed/draft/final carry the FULL text each time (replace semantics), which
// makes revision trivial: just send the new best text and the block reprints.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiCyan  = "\x1b[36m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiGray  = "\x1b[90m"
)

type event struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	State string `json:"state"`
}

type view struct {
	w         *bufio.Writer
	width     int
	prevLines int

	committed string
	draft     string
	status    string
	done      bool

	start      time.Time
	lastUpdate time.Time
	commits    int
	drafts     int
	firstDraft time.Duration // latency to first visible word
}

func main() {
	connect := flag.String("connect", "", "read events from this unix socket instead of stdin")
	width := flag.Int("width", detectWidth(), "wrap width in columns")
	flag.Parse()

	v := &view{w: bufio.NewWriter(os.Stdout), width: *width, start: time.Now()}

	var sc *bufio.Scanner
	if *connect != "" {
		conn, err := net.Dial("unix", *connect)
		if err != nil {
			fmt.Fprintf(os.Stderr, "visualizer: cannot connect %s: %v\n", *connect, err)
			os.Exit(1)
		}
		defer conn.Close()
		sc = bufio.NewScanner(conn)
	} else {
		sc = bufio.NewScanner(os.Stdin)
	}
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	v.render()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// tolerate non-JSON lines (e.g. stray logs): show them as info
			v.log(ansiGray, line)
			continue
		}
		v.apply(e)
	}
	v.w.Flush()
	fmt.Fprintln(os.Stderr) // leave the final block on screen
}

func (v *view) apply(e event) {
	now := time.Now()
	switch e.Type {
	case "committed":
		v.committed = e.Text
		v.commits++
		v.lastUpdate = now
		if v.firstDraft == 0 && e.Text != "" {
			v.firstDraft = now.Sub(v.start)
		}
	case "draft":
		v.draft = e.Text
		v.drafts++
		v.lastUpdate = now
		if v.firstDraft == 0 && e.Text != "" {
			v.firstDraft = now.Sub(v.start)
		}
	case "final":
		v.committed = e.Text
		v.draft = ""
		v.done = true
		v.lastUpdate = now
	case "status":
		v.status = e.State
		if v.status == "" {
			v.status = e.Text
		}
	case "reset":
		v.committed, v.draft, v.status, v.done = "", "", "", false
		v.commits, v.drafts, v.firstDraft = 0, 0, 0
		v.start = now
	case "info":
		v.log(ansiGray, e.Text)
		return
	case "error":
		v.log(ansiRed, "ERROR: "+e.Text)
		return
	default:
		v.log(ansiGray, "?"+e.Type)
		return
	}
	v.render()
}

// log prints a permanent line above the live block, then redraws the block.
func (v *view) log(color, msg string) {
	v.clear()
	fmt.Fprintf(v.w, "%s%s%s\n", color, msg, ansiReset)
	v.render()
}

// clear moves the cursor to the top of the previous block and erases downward.
func (v *view) clear() {
	if v.prevLines > 0 {
		fmt.Fprintf(v.w, "\x1b[%dA", v.prevLines)
	}
	fmt.Fprint(v.w, "\r\x1b[0J")
	v.prevLines = 0
}

func (v *view) render() {
	v.clear()

	statusColor := ansiCyan
	switch v.status {
	case "recording":
		statusColor = ansiGreen
	case "idle", "done":
		statusColor = ansiGray
	}
	st := v.status
	if v.done {
		st = "done"
		statusColor = ansiGreen
	}
	if st == "" {
		st = "…"
	}
	header := fmt.Sprintf("%s● %s%s", statusColor, st, ansiReset)

	body := v.wrap()

	dl := "—"
	if !v.lastUpdate.IsZero() {
		dl = fmt.Sprintf("%dms", time.Since(v.lastUpdate).Milliseconds())
	}
	fd := "—"
	if v.firstDraft > 0 {
		fd = strconv.FormatInt(v.firstDraft.Milliseconds(), 10) + "ms"
	}
	footer := fmt.Sprintf("%s[t=%.1fs  first-word=%s  commits=%d  drafts=%d  Δlast=%s]%s",
		ansiGray, time.Since(v.start).Seconds(), fd, v.commits, v.drafts, dl, ansiReset)

	lines := append([]string{header}, body...)
	lines = append(lines, footer)
	fmt.Fprint(v.w, strings.Join(lines, "\n"))
	v.prevLines = countPhysical(lines, v.width)
	v.w.Flush()
}

type tok struct {
	s     string
	draft bool
}

// wrap greedily word-wraps committed (normal) + draft (dim) to the width,
// coloring each word by origin so corrections read clearly.
func (v *view) wrap() []string {
	var toks []tok
	for _, w := range strings.Fields(v.committed) {
		toks = append(toks, tok{w, false})
	}
	for _, w := range strings.Fields(v.draft) {
		toks = append(toks, tok{w, true})
	}
	if len(toks) == 0 {
		return []string{ansiGray + "(waiting for audio…)" + ansiReset}
	}

	var lines []string
	var cur strings.Builder
	curLen := 0
	for _, t := range toks {
		wlen := len([]rune(t.s))
		add := wlen
		if curLen > 0 {
			add++ // leading space
		}
		if curLen > 0 && curLen+add > v.width {
			lines = append(lines, cur.String())
			cur.Reset()
			curLen = 0
			add = wlen
		}
		if curLen > 0 {
			cur.WriteByte(' ')
		}
		if t.draft {
			cur.WriteString(ansiDim + t.s + ansiReset)
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

// countPhysical counts wrapped terminal rows, ignoring ANSI escape sequences.
func countPhysical(lines []string, width int) int {
	n := 0
	for _, l := range lines {
		visible := visibleLen(l)
		if visible == 0 {
			n++
			continue
		}
		rows := (visible + width - 1) / width
		if rows < 1 {
			rows = 1
		}
		n += rows
	}
	return n
}

func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

func detectWidth() int {
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 20 {
			return n
		}
	}
	return 100
}
