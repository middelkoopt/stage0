package console

import "io"

// Plain returns a writer that passes text on to w without terminal escape
// sequences (colours, cursor moves, OSC titles such as systemd's) or other
// control characters but newline, carriage return and tab, so logs read
// and search as text. It keeps its place across writes: a sequence may be
// split between two.
func Plain(w io.Writer) io.Writer { return &plain{w: w} }

type plain struct {
	w     io.Writer
	state int // 0 text, 1 after ESC, 2 in CSI, 3 in OSC, 4 OSC's ESC, 5 ESC ( and its byte
}

func (p *plain) Write(b []byte) (int, error) {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch p.state {
		case 0:
			switch {
			case c == 0x1b:
				p.state = 1
			case c >= 0x20 || c == '\n' || c == '\r' || c == '\t':
				if c != 0x7f {
					out = append(out, c)
				}
			}
		case 1: // ESC: [ starts CSI, ] starts OSC, ( ) etc take one more
			switch c {
			case '[':
				p.state = 2
			case ']':
				p.state = 3
			case '(', ')', '*', '+', '#', '%':
				p.state = 5
			default:
				p.state = 0
			}
		case 2: // CSI: parameters, then a final byte 0x40–0x7e
			if c >= 0x40 && c <= 0x7e {
				p.state = 0
			}
		case 3: // OSC: until BEL or ESC \
			switch c {
			case 0x07:
				p.state = 0
			case 0x1b:
				p.state = 4
			}
		case 4:
			p.state = 0
		case 5: // the one byte after ESC (
			p.state = 0
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}
