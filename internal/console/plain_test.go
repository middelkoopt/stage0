package console

import (
	"bytes"
	"testing"
)

func TestPlain(t *testing.T) {
	var out bytes.Buffer
	p := Plain(&out)
	// systemd's status lines: colours, cursor moves, an OSC title, split
	// across writes.
	for _, w := range []string{
		"[\x1b[0;32m  OK  \x1b[0m] Started sshd\r\n",
		"\x1b]3008;start=abc;type=boot\x1b\\k52 login: ",
		"\x1b[0;1;3", "1m*\x1b[0m] Job running\x1b[K\n",
		"bell\x07 and \x1b(B charset\n",
	} {
		if n, err := p.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	want := "[  OK  ] Started sshd\r\nk52 login: *] Job running\nbell and  charset\n"
	if out.String() != want {
		t.Errorf("got  %q\nwant %q", out.String(), want)
	}
}
