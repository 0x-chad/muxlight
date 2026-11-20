package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamvduke/pst"
	"github.com/paipaiio/muxlight/internal/status"
)

// fakeHC writes an hc stand-in. `ls` answers only for the label
// herdr.window=42 and prints the address t9; `report` appends its arguments to
// a log, and fails when the file "gone" exists, as hc does for a terminal that
// has been closed.
func fakeHC(t *testing.T) (hc, log, gone string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	gone = filepath.Join(dir, "gone")
	hc = filepath.Join(dir, "hc")
	script := `#!/bin/sh
case "$1" in
ls)
  for a in "$@"; do
    if [ "$a" = "herdr.window=42" ]; then
      echo "ls $*" >> ` + log + `
      echo '{"terminals":[{"address":"t9"}]}'
      exit 0
    fi
  done
  echo '{"terminals":[]}'
  ;;
report)
  [ -e ` + gone + ` ] && exit 1
  echo "$*" >> ` + log + `
  ;;
esac
`
	if err := os.WriteFile(hc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return hc, log, gone
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func working() status.Summary { return status.Summary{State: pst.StateWorking, App: "codex"} }

func TestReportsToTerminalFoundByLabel(t *testing.T) {
	hc, log, _ := fakeHC(t)
	p := &Publisher{hc: hc, window: "42"}
	p.send(working())
	p.send(working())

	got := lines(t, log)
	var reports, lookups int
	for _, l := range got {
		switch {
		case strings.HasPrefix(l, "report"):
			reports++
			if !strings.Contains(l, "--terminal t9") {
				t.Errorf("report not sent to the labelled terminal: %q", l)
			}
		case strings.HasPrefix(l, "ls"):
			lookups++
		}
	}
	if reports != 2 {
		t.Errorf("reports = %d, want 2 (%v)", reports, got)
	}
	if lookups != 1 {
		t.Errorf("lookups = %d, want 1: the address is kept between reports", lookups)
	}
}

func TestNoLabelledTerminalMeansNoReport(t *testing.T) {
	hc, log, _ := fakeHC(t)
	p := &Publisher{hc: hc, window: "7"}
	p.send(working())
	if got := lines(t, log); len(got) != 0 {
		t.Errorf("sent something with no terminal to send it to: %v", got)
	}
}

func TestLooksUpAgainAfterAReportFails(t *testing.T) {
	hc, log, gone := fakeHC(t)
	p := &Publisher{hc: hc, window: "42"}
	p.send(working())
	if err := os.WriteFile(gone, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p.send(working()) // report fails: the terminal is gone
	if p.resolved != "" {
		t.Fatalf("address kept after its report failed: %q", p.resolved)
	}
	os.Remove(gone)
	p.send(working())

	var lookups int
	for _, l := range lines(t, log) {
		if strings.HasPrefix(l, "ls") {
			lookups++
		}
	}
	if lookups != 2 {
		t.Errorf("lookups = %d, want 2 (initial and after the failure)", lookups)
	}
}

func TestHerdrStartedAgentReportsStraightToItsTerminal(t *testing.T) {
	hc, log, _ := fakeHC(t)
	p := &Publisher{hc: hc, target: "t3"}
	p.send(working())
	for _, l := range lines(t, log) {
		if strings.HasPrefix(l, "ls") {
			t.Errorf("looked up a terminal when HC_TERMINAL_ID named one: %q", l)
		}
	}
	if got := lines(t, log); len(got) != 1 || !strings.Contains(got[0], "--terminal t3") {
		t.Errorf("got %v, want one report to t3", got)
	}
}

// A terminal attached after the agent last changed state must still be told
// the current status, without waiting for the next change.
func TestResendsCurrentStatusOnceATerminalAppears(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	up := filepath.Join(dir, "up")
	hc := filepath.Join(dir, "hc")
	script := `#!/bin/sh
case "$1" in
ls)
  if [ -e ` + up + ` ]; then echo '{"terminals":[{"address":"t9"}]}'; else echo '{"terminals":[]}'; fi
  ;;
report)
  echo "$*" >> ` + log + `
  ;;
esac
`
	if err := os.WriteFile(hc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Publisher{
		hc:     hc,
		window: "42",
		retry:  20 * time.Millisecond,
		latest: make(chan status.Summary, 1),
		done:   make(chan struct{}),
	}
	go p.loop()
	defer func() { close(p.latest); <-p.done }()

	p.Publish(working()) // no terminal yet: nothing to send it to
	time.Sleep(150 * time.Millisecond)
	if got := lines(t, log); len(got) != 0 {
		t.Fatalf("reported with no terminal: %v", got)
	}

	if err := os.WriteFile(up, nil, 0o644); err != nil { // the tab is attached
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := lines(t, log); len(got) > 0 {
			if !strings.Contains(got[0], "state=working") || !strings.Contains(got[0], "--terminal t9") {
				t.Fatalf("resent the wrong thing: %v", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("status was never resent after the terminal appeared")
}
