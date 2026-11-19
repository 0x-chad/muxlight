// Package herdr mirrors a pane's OSC 7501 summary into herdr-core, so a herdr
// tab attached to this tmux window shows the agent's real status.
//
// herdr reads status from a program's own terminal, which tmux consumes before
// herdr can see it. `hc report` is herdr-core's supported side channel for
// exactly this case, so muxlight forwards each summary as it happens. The
// terminal is addressed by name: a pane in tmux window "audit" reports to the
// herdr terminal "sv-audit", matching the grouped-session tabs.
//
// Disabled unless MUXLIGHT_HC names an hc binary.
package herdr

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/paipaiio/muxlight/internal/status"
)

const timeout = 2 * time.Second

// prefix forms the herdr terminal name from the pane's tmux window id: the
// view session backing that tab, "herdr-view/@129". The window id is used
// rather than its name because a window linked into a view belongs to both
// sessions, so a pane cannot tell which session owns it, and because names
// change under automatic-rename while ids do not. Deliberately not settable:
// it must equal the prefix the view sessions are created with, and a mismatch
// fails silently because report errors are ignored. Change it in both places.
const prefix = "herdr-view/"

// Publisher reports summaries for one pane. The zero value is disabled.
//
// Publish never blocks: it runs on its own goroutine and keeps only the latest
// summary, because Publish is called from the shim's output path and a stalled
// hc would stall the agent's screen.
type Publisher struct {
	hc     string // hc binary; empty disables
	target string // herdr terminal name
	latest chan status.Summary
	done   chan struct{}
}

// New returns a Publisher for pane. It finds hc on PATH; MUXLIGHT_HC names a
// different binary, and MUXLIGHT_HC=off disables mirroring. A Publisher is
// disabled when hc is absent or the window name cannot be read.
func New(pane string) *Publisher {
	hc := resolveHC()
	if hc == "" || pane == "" {
		return &Publisher{}
	}
	win := tmuxWindowID(pane)
	if win == "" {
		return &Publisher{}
	}
	p := &Publisher{
		hc:     hc,
		target: prefix + win,
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
	for s := range p.latest {
		p.send(s)
	}
}

// send runs one hc report. Errors are ignored: a status mirror must never
// disturb the agent, and hc may legitimately have no terminal for this window.
func (p *Publisher) send(s status.Summary) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.hc, "report", body(s), "--terminal", p.target)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	_ = cmd.Run()
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
	return strings.TrimSpace(string(out))
}

