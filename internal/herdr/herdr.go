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

// Prefix is prepended to the tmux window name to form the herdr terminal name.
var Prefix = envOr("MUXLIGHT_HC_PREFIX", "sv-")

// Publisher reports summaries for one pane. The zero value is disabled.
type Publisher struct {
	hc     string // hc binary; empty disables
	target string // herdr terminal name
}

// New returns a Publisher for pane, or a disabled one when MUXLIGHT_HC is
// unset or the window name cannot be read.
func New(pane string) *Publisher {
	hc := os.Getenv("MUXLIGHT_HC")
	if hc == "" || pane == "" {
		return &Publisher{}
	}
	win := tmuxWindowName(pane)
	if win == "" {
		return &Publisher{}
	}
	return &Publisher{hc: hc, target: Prefix + win}
}

// Enabled reports whether Publish will do anything.
func (p *Publisher) Enabled() bool { return p != nil && p.hc != "" }

// Publish sends one summary. Errors are ignored: a status mirror must never
// disturb the agent, and hc may legitimately have no terminal for this window.
func (p *Publisher) Publish(s status.Summary) {
	if !p.Enabled() {
		return
	}
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

func tmuxWindowName(pane string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#W").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
