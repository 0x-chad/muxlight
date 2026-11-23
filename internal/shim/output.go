package shim

import (
	"io"
	"sync"
)

// Output is the program's output on its way to the pane, plus a way to add
// bytes of muxlight's own. Inject never splits the program's output: bytes go
// out only where the stream sits between sequences and characters, so an
// escape sequence or a UTF-8 character the program is half way through writing
// stays whole. Injected bytes that cannot go now wait for the next such point;
// a newer injection replaces a waiting one.
type Output struct {
	mu      sync.Mutex
	w       io.Writer
	b       boundary
	pending []byte
}

// NewOutput wraps w, the pane.
func NewOutput(w io.Writer) *Output { return &Output{w: w} }

// Write passes program output through unchanged.
func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.w.Write(p)
	o.b.feed(p[:n])
	if err == nil && o.pending != nil && o.b.between() {
		_, err = o.w.Write(o.pending)
		o.pending = nil
	}
	return n, err
}

// Inject writes b now if the program's output is between sequences, else
// as soon as it is.
func (o *Output) Inject(b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.b.between() {
		_, _ = o.w.Write(b)
		o.pending = nil
		return
	}
	o.pending = append([]byte(nil), b...)
}

// boundary follows just enough of a VT parser to know whether the stream is
// between sequences: ESC sequences, CSI, the string sequences (OSC, DCS, SOS,
// PM, APC) and multi-byte UTF-8 characters.
type boundary struct {
	state byte
	utf8  int // continuation bytes still expected
}

const (
	bGround byte = iota
	bEsc
	bEscInter // ESC then intermediates, until a final byte
	bCSI
	bString
	bStringEsc
)

func (b *boundary) between() bool { return b.state == bGround && b.utf8 == 0 }

func (b *boundary) feed(p []byte) {
	for _, c := range p {
		switch b.state {
		case bGround:
			switch {
			case c == 0x1b:
				b.state, b.utf8 = bEsc, 0
			case c >= 0xf0:
				b.utf8 = 3
			case c >= 0xe0:
				b.utf8 = 2
			case c >= 0xc0:
				b.utf8 = 1
			case c >= 0x80:
				if b.utf8 > 0 {
					b.utf8--
				}
			default:
				b.utf8 = 0
			}
		case bEsc:
			b.afterEsc(c)
		case bEscInter:
			switch {
			case c == 0x1b:
				b.state = bEsc
			case c >= 0x20 && c <= 0x2f:
			default:
				b.state = bGround
			}
		case bCSI:
			switch {
			case c == 0x1b:
				b.state = bEsc
			case c == 0x18 || c == 0x1a || (c >= 0x40 && c <= 0x7e):
				b.state = bGround
			}
		case bString:
			switch c {
			case 0x07, 0x18, 0x1a:
				b.state = bGround
			case 0x1b:
				b.state = bStringEsc
			}
		case bStringEsc:
			if c == '\\' {
				b.state = bGround
			} else {
				b.afterEsc(c)
			}
		}
	}
}

func (b *boundary) afterEsc(c byte) {
	switch {
	case c == '[':
		b.state = bCSI
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		b.state = bString
	case c == 0x1b:
		b.state = bEsc
	case c >= 0x20 && c <= 0x2f:
		b.state = bEscInter
	default:
		b.state = bGround
	}
}
