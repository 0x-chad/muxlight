// Package herdr mirrors a pane's OSC 7501 summary into herdr, so a herdr tab
// attached to this tmux window shows the agent's real status.
//
// herdr reads status from what a program writes to its terminal, and tmux
// consumes OSC 7501 before herdr's terminal sees it. So muxlight writes the
// summary again wrapped in tmux's passthrough sequence (allow-passthrough must
// be on), which tmux hands unchanged to the clients showing the pane: one of
// them is the herdr view terminal running `tmux attach`, and herdr reads the
// report there as if the agent had written it.
//
// Passthrough reaches only clients attached at that moment, so the summary is
// sent again whenever a herdr terminal for this window appears. That terminal
// is found by label: a pane in tmux window @129 belongs to the herdr terminal
// labelled herdr.window=129.
package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/paipaiio/muxlight/internal/status"
)

const timeout = 2 * time.Second

// How often to look for this window's herdr terminal, backing off to
// maxRetry; a status change or Refresh starts again at firstRetry.
const (
	firstRetry = 3 * time.Second
	maxRetry   = 60 * time.Second
)

// windowLabel is the label herdr-manage puts on a view terminal, carrying the
// tmux window id without its "@" ("129" for window @129). The window id
// rather than the name, because a window linked into a view belongs to both
// sessions, and because names change under automatic-rename while ids do not.
const windowLabel = "herdr.window"

// Publisher mirrors summaries for one pane. The zero value is disabled.
//
// Publish never blocks: it runs on its own goroutine and keeps only the latest
// summary, because Publish is called from the shim's output path and a stalled
// hc would stall the agent's screen.
type Publisher struct {
	hc      string       // hc binary, for finding the herdr terminal; empty disables
	window  string       // tmux window id without "@"
	inject  func([]byte) // writes to the pane, between the program's sequences
	retry   time.Duration
	latest  chan status.Summary
	refresh chan struct{}
	done    chan struct{}

	sentTo string // herdr terminal the last summary reached; "" when none
}

// New returns a Publisher for pane that writes through inject. It finds hc on
// PATH; MUXLIGHT_HC names a different binary, and MUXLIGHT_HC=off disables
// mirroring.
func New(pane string, inject func([]byte)) *Publisher {
	hc := resolveHC()
	if hc == "" || pane == "" || inject == nil {
		return &Publisher{}
	}
	window := tmuxWindowID(pane)
	if window == "" {
		return &Publisher{}
	}
	p := &Publisher{hc: hc, window: window, inject: inject}
	p.start()
	return p
}

func (p *Publisher) start() {
	p.latest = make(chan status.Summary, 1)
	p.refresh = make(chan struct{}, 1)
	p.done = make(chan struct{})
	go p.loop()
}

// Enabled reports whether Publish will do anything.
func (p *Publisher) Enabled() bool { return p != nil && p.hc != "" }

// Publish queues one summary, replacing any still-pending one. It never blocks.
func (p *Publisher) Publish(s status.Summary) {
	if !p.Enabled() {
		return
	}
	for {
		select {
		case p.latest <- s:
			return
		default:
			select {
			case <-p.latest: // drop the stale one
			default:
			}
		}
	}
}

// Refresh looks for the herdr terminal now, and sends the current summary to
// it even if it was already told. muxlight calls it on SIGWINCH, which a herdr
// view attaching causes and herdr-manage sends, so a new tab is right at once.
func (p *Publisher) Refresh() {
	if !p.Enabled() {
		return
	}
	select {
	case p.refresh <- struct{}{}:
	default:
	}
}

// Close publishes final after everything already queued, then stops.
func (p *Publisher) Close(final status.Summary) {
	if !p.Enabled() {
		return
	}
	close(p.latest)
	<-p.done
	if p.sentTo != "" {
		p.write(final)
	}
}

func (p *Publisher) loop() {
	defer close(p.done)
	wait := p.firstRetry()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var last status.Summary
	have := false
	for {
		force := false
		select {
		case s, ok := <-p.latest:
			if !ok {
				return
			}
			last, have = s, true
			if p.sentTo != "" {
				p.write(s)
			} else if t := p.lookup(); t != "" {
				p.write(s)
				p.sentTo = t
			}
			wait = p.firstRetry()
			timer.Reset(wait)
			continue
		case <-p.refresh:
			force = true
			wait = p.firstRetry()
		case <-timer.C:
			if wait *= 2; wait > maxRetry {
				wait = maxRetry
			}
		}
		// A tab shown, hidden or shown again gets a new terminal; the one we
		// told may be gone. Tell a new one what the agent is doing now.
		t := p.lookup()
		if have && t != "" && (force || t != p.sentTo) {
			p.write(last)
		}
		p.sentTo = t
		timer.Reset(wait)
	}
}

func (p *Publisher) firstRetry() time.Duration {
	if p.retry > 0 {
		return p.retry
	}
	return firstRetry
}

func (p *Publisher) write(s status.Summary) {
	p.inject(Passthrough(body(s)))
}

// Passthrough wraps an OSC 7501 report body so tmux passes it, unchanged, to
// the terminals attached to the pane.
func Passthrough(body string) []byte {
	osc := "\x1b]7501;" + body + "\x1b\\"
	return []byte("\x1bPtmux;" + strings.ReplaceAll(osc, "\x1b", "\x1b\x1b") + "\x1b\\")
}

// lookup returns the address of the herdr terminal labelled with this pane's
// window id, or "" when there is none.
func (p *Publisher) lookup() string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.hc, "ls", "--json",
		"--label", windowLabel+"="+p.window).Output()
	if err != nil {
		debugf("lookup of %s=%s failed: %v", windowLabel, p.window, err)
		return ""
	}
	var list struct {
		Terminals []struct {
			Address string `json:"address"`
		} `json:"terminals"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		debugf("lookup of %s=%s: unreadable answer: %v", windowLabel, p.window, err)
		return ""
	}
	if len(list.Terminals) == 0 {
		return ""
	}
	return list.Terminals[0].Address
}

// body renders the summary as an OSC 7501 report body, as a program would
// send it. An empty summary clears the record.
func body(s status.Summary) string {
	if s.Empty() {
		return "state=clear"
	}
	b := "state=" + string(s.State)
	if s.Kind != "" {
		b += ":kind=" + string(s.Kind)
	}
	if app := sanitize(s.App); app != "" {
		b += ":app=" + app
	}
	return b
}

// sanitize keeps only characters the spec allows in a value.
func sanitize(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune("_.,+/=-", r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveHC returns the hc binary to use, or "" when mirroring is off.
func resolveHC() string {
	switch v := os.Getenv("MUXLIGHT_HC"); v {
	case "":
		path, err := exec.LookPath("hc")
		if err != nil {
			return ""
		}
		return path
	case "off", "0", "no", "false":
		return ""
	default:
		return v
	}
}

// tmuxWindowID returns the pane's window id without "@" ("129"), tmux's
// stable identity for the window, independent of name or session membership.
func tmuxWindowID(pane string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#{window_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "@")
}

// debugf appends one line to the file named by MUXLIGHT_DEBUG, if any. It is a
// file, not stderr, because stderr here is the user's terminal.
func debugf(format string, args ...any) {
	path := os.Getenv("MUXLIGHT_DEBUG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s herdr: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
