package agentpty

import (
	"bytes"
	"testing"
)

func TestTermQueryTapAnswers(t *testing.T) {
	var out, reply bytes.Buffer
	w := newTermQueryTap(&out, &reply)
	// Muse Code's startup burst, with the OSC 4 query and the DSR split across writes.
	chunks := []string{
		"\x1b]10;?\x07\x1b]11;?\x07\x1b]4;3",
		";?\x07\x1b[?2004h\x1b[?u\x1b[c\x1b[",
		"6n hello",
	}
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := out.String(), chunks[0]+chunks[1]+chunks[2]; got != want {
		t.Fatalf("output altered: %q", got)
	}
	want := "\x1b]10;rgb:ffff/ffff/ffff\x1b\\" +
		"\x1b]11;rgb:0000/0000/0000\x1b\\" +
		"\x1b]4;3;rgb:8080/8080/8080\x1b\\" +
		"\x1b[?0u" +
		"\x1b[?62;22c" +
		"\x1b[1;1R"
	if reply.String() != want {
		t.Fatalf("replies:\n got %q\nwant %q", reply.String(), want)
	}
}

func TestTermQueryTapIgnoresPlainOutput(t *testing.T) {
	var out, reply bytes.Buffer
	w := newTermQueryTap(&out, &reply)
	_, _ = w.Write([]byte("\x1b[?25l\x1b[2Jplain text 6n c ?u\x1b[0m"))
	if reply.Len() != 0 {
		t.Fatalf("unexpected reply %q", reply.String())
	}
}
