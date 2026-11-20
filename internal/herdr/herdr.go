// Package herdr mirrors a pane's OSC 7501 summary into herdr-core, so a herdr
// tab attached to this tmux window shows the agent's real status.
//
// herdr reads status from a program's own terminal, which tmux consumes before
// herdr can see it. `hc report` is herdr-core's supported side channel for
// exactly this case, so muxlight forwards each summary as it happens. The
// terminal is found by label: a pane in tmux window @129 reports to the herdr
// terminal labelled herdr.window=129.
//
// Disabled unless MUXLIGHT_HC names an hc binary.
package herdr

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/paipaiio/muxlight/internal/status"
)

const timeout = 2 * time.Second

// While no herdr terminal carries this pane's window label, look again this
// often, backing off to maxRetry. Reports only happen on a change, so a tab
// attached after the agent last changed state would otherwise show nothing
// until its next transition, which for an idle agent may be hours away.
const (
	firstRetry = 3 * time.Second
	maxRetry   = 60 * time.Second
)

// windowLabel is the label herdr-focus puts on a view terminal, carrying the
// tmux window id without its "@" ("129" for window @129). The terminal is
// found by this label, not by name, so its name is free to be readable: herdr
// clients show it as the agent's name. The window id rather than the window
// name because a window linked into a view belongs to both sessions, so a pane
// cannot tell which session owns it, and because names change under
// automatic-rename while ids do not.
const windowLabel = "herdr.window"

// Publisher reports summaries for one pane. The zero value is disabled.
//
// Publish never blocks: it runs on its own goroutine and keeps only the latest
// summary, because Publish is called from the shim's output path and a stalled
// hc would stall the agent's screen.
type Publisher struct {
	hc       string        // hc binary; empty disables
	target   string        // herdr terminal address; resolved lazily when empty
	window   string        // tmux window id without "@", used to find the terminal
	resolved string        // address found by label, kept until a report to it fails
	retry    time.Duration // first wait between lookups while unresolved; zero uses firstRetry
	latest   chan status.Summary
	done     chan struct{}
}

// New returns a Publisher for pane. It finds hc on PATH; MUXLIGHT_HC names a
// different binary, and MUXLIGHT_HC=off disables mirroring. A Publisher is
// disabled when hc is absent, or when no herdr terminal can be addressed.
//
// There are two ways an agent reaches herdr. When herdr started it, the agent
// is already inside a herdr terminal and HC_TERMINAL_ID names it; report
// straight there. Otherwise the agent is in a tmux window someone else made,
// and herdr sees it through a view terminal carrying a herdr.window label with
// the window id. That terminal may not exist yet, or may come and go as the tab
// is promoted and demoted, so it is looked up when a report is sent.
func New(pane string) *Publisher {
	hc := resolveHC()
	if hc == "" || pane == "" {
		return &Publisher{}
	}
	target := os.Getenv("HC_TERMINAL_ID")
	window := ""
	if target == "" {
		window = tmuxWindowID(pane)
		if window == "" {
			return &Publisher{}
		}
	}
	p := &Publisher{
		hc:     hc,
		target: target,
		window: window,
		latest: make(chan status.Summary, 1),
		done:   make(chan struct{}),
	}
	go p.loop()
	return p
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

// Close publishes final after everything already queued, then stops.
func (p *Publisher) Close(final status.Summary) {
	if !p.Enabled() {
		return
	}
	close(p.latest)
	<-p.done
	p.send(final)
}

func (p *Publisher) loop() {
	defer close(p.done)
	wait := p.firstRetry()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var last status.Summary
	have := false
	for {
		select {
		case s, ok := <-p.latest:
			if !ok {
				return
			}
			last, have = s, true
			p.send(s)
			wait = p.firstRetry()
		case <-timer.C:
			// Nothing to resend, or nowhere it needs to go: a herdr-started
			// agent has a fixed target, and a resolved one is already told.
			if have && p.target == "" && p.resolved == "" && p.lookup() != "" {
				p.send(last)
			}
			if wait *= 2; wait > maxRetry {
				wait = maxRetry
			}
			timer.Reset(wait)
		}
	}
}

func (p *Publisher) firstRetry() time.Duration {
	if p.retry > 0 {
		return p.retry
	}
	return firstRetry
}

// send runs one hc report. Errors are ignored: a status mirror must never
// disturb the agent, and hc may legitimately have no terminal for this window.
func (p *Publisher) send(s status.Summary) {
	target := p.target
	fromLabel := target == ""
	if fromLabel {
		target = p.lookup()
		if target == "" {
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.hc, "report", body(s), "--terminal", target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil && fromLabel {
		// The terminal went away (the tab was demoted); look it up afresh next time.
		p.resolved = ""
	}
}

// lookup returns the address of the terminal labelled with this pane's window
// id, or "" when there is none. A found address is kept until a report to it
// fails, so a status change costs one hc call, not two.
func (p *Publisher) lookup() string {
	if p.resolved != "" {
		return p.resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.hc, "ls", "--json",
		"--label", windowLabel+"="+p.window).Output()
	if err != nil {
		return ""
	}
	var list struct {
		Terminals []struct {
			Address string `json:"address"`
		} `json:"terminals"`
	}
	if json.Unmarshal(out, &list) != nil || len(list.Terminals) == 0 {
		return ""
	}
	p.resolved = list.Terminals[0].Address
	return p.resolved
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

// tmuxWindowID returns the pane's window id ("@129"), tmux's stable identity
// for the window, independent of name or session membership.
func tmuxWindowID(pane string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#{window_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "@")
}
