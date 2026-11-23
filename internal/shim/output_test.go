package shim

import (
	"bytes"
	"testing"
)

func TestInjectWaitsForTheEndOfASequence(t *testing.T) {
	cases := []struct {
		name  string
		first string // written before the injection
		rest  string // written after
	}{
		{"csi", "a\x1b[3", "1mb"},
		{"osc", "\x1b]0;tit", "le\x07c"},
		{"osc st", "\x1b]8;;http://x\x1b", "\\d"},
		{"dcs", "\x1bPq#0", "\x1b\\e"},
		{"charset", "\x1b(", "Bf"},
		{"utf8", "\xe2\x94", "\x80g"},
		{"esc", "\x1b", "7h"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		o := NewOutput(&out)
		_, _ = o.Write([]byte(c.first))
		o.Inject([]byte("<X>"))
		if out.String() != c.first {
			t.Errorf("%s: injected inside a sequence: %q", c.name, out.String())
		}
		_, _ = o.Write([]byte(c.rest))
		if want := c.first + c.rest + "<X>"; out.String() != want {
			t.Errorf("%s: got %q, want %q", c.name, out.String(), want)
		}
	}
}

func TestInjectGoesOutAtOnceBetweenSequences(t *testing.T) {
	var out bytes.Buffer
	o := NewOutput(&out)
	_, _ = o.Write([]byte("plain \x1b[0m text \xe2\x94\x80"))
	o.Inject([]byte("<X>"))
	if want := "plain \x1b[0m text \xe2\x94\x80<X>"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestOnlyTheNewestWaitingInjectionIsSent(t *testing.T) {
	var out bytes.Buffer
	o := NewOutput(&out)
	_, _ = o.Write([]byte("\x1b["))
	o.Inject([]byte("<old>"))
	o.Inject([]byte("<new>"))
	_, _ = o.Write([]byte("m"))
	if want := "\x1b[m<new>"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}
