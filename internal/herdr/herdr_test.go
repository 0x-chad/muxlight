package herdr

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/adamvduke/pst"
	"github.com/paipaiio/muxlight/internal/status"
)

// fakeHC writes an hc stand-in whose `ls` finds, for the label
// herdr.window=42, the address written in the file it returns ("" for none).
func fakeHC(t *testing.T) (hc, addr string) {
	t.Helper()
	dir := t.TempDir()
	addr = filepath.Join(dir, "addr")
	hc = filepath.Join(dir, "hc")
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "herdr.window=42" ] && [ -s ` + addr + ` ]; then
    echo "{\"terminals\":[{\"address\":\"$(cat ` + addr + `)\"}]}"
    exit 0
  fi
done
echo '{"terminals":[]}'
`
	if err := os.WriteFile(hc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return hc, addr
}

func setAddr(t *testing.T, path, v string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sink records what the publisher writes to the pane.
type sink struct {
	mu  sync.Mutex
	got []string
}

func (s *sink) inject(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, string(b))
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *sink) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %d writes, want %d: %q", len(s.all()), n, s.all())
	return nil
}

func working() status.Summary { return status.Summary{State: pst.StateWorking, App: "codex"} }
func done() status.Summary    { return status.Summary{State: pst.StateDone, App: "codex"} }

func newTest(t *testing.T) (*Publisher, *sink, string) {
	hc, addr := fakeHC(t)
	s := &sink{}
	p := &Publisher{hc: hc, window: "42", inject: s.inject, retry: 20 * time.Millisecond}
	p.start()
	return p, s, addr
}

func TestPassthroughWrapsTheReportForTmux(t *testing.T) {
	got := string(Passthrough("state=working:app=codex"))
	want := "\x1bPtmux;\x1b\x1b]7501;state=working:app=codex\x1b\x1b\\\x1b\\"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSendsWhenAHerdrTerminalExists(t *testing.T) {
	p, s, addr := newTest(t)
	setAddr(t, addr, "t9")
	p.Publish(working())
	got := s.waitFor(t, 1)
	if got[0] != string(Passthrough("state=working:app=codex")) {
		t.Fatalf("wrote %q", got[0])
	}
	p.Close(done())
	if last := s.all()[len(s.all())-1]; last != string(Passthrough("state=done:app=codex")) {
		t.Fatalf("final write %q", last)
	}
}

func TestWritesNothingWithoutAHerdrTerminal(t *testing.T) {
	p, s, _ := newTest(t)
	p.Publish(working())
	time.Sleep(100 * time.Millisecond)
	p.Close(done())
	if got := s.all(); len(got) != 0 {
		t.Fatalf("wrote %q with no herdr tab", got)
	}
}

func TestResendsOnceATerminalAppears(t *testing.T) {
	p, s, addr := newTest(t)
	p.Publish(working())
	time.Sleep(50 * time.Millisecond)
	setAddr(t, addr, "t9")
	s.waitFor(t, 1)
	// Already told: no repeats while the same terminal stays.
	time.Sleep(150 * time.Millisecond)
	if got := s.all(); len(got) != 1 {
		t.Fatalf("repeated to the same terminal: %q", got)
	}
	// Hidden and shown again: a new terminal hears the status again.
	setAddr(t, addr, "t10")
	s.waitFor(t, 2)
	p.Close(done())
}

func TestRefreshResendsToTheSameTerminal(t *testing.T) {
	p, s, addr := newTest(t)
	setAddr(t, addr, "t9")
	p.Publish(working())
	s.waitFor(t, 1)
	p.Refresh()
	s.waitFor(t, 2)
	p.Close(done())
}

func TestDisabledPublisherDoesNothing(t *testing.T) {
	var p Publisher
	p.Publish(working())
	p.Refresh()
	p.Close(done())
	if p.Enabled() {
		t.Fatal("zero Publisher is enabled")
	}
}
