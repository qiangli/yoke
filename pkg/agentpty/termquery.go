package agentpty

import (
	"bytes"
	"io"
	"regexp"
)

// HEADLESS TERMINAL QUERIES.
//
// A headless agent gets a PTY but no terminal emulator behind it, so any query
// a TUI sends to learn about its terminal goes unanswered. Most TUIs time out and
// carry on; Muse Code (1.3.0) does not — measured 2026-09-23, it sends
//
//	OSC 10/11 ;?   (foreground/background colour)   OSC 4;N;?  (palette)
//	CSI ? u        (kitty keyboard flags)           CSI c      (primary DA)
//	CSI 6n         (cursor position)
//
// and exits a moment later when nothing replies, which made it unsteerable.
// termQueryTap answers them the way a plain xterm-class terminal would, so the
// TUI stays up. It only runs on the headless branch: with a real terminal on
// the parent, that terminal answers and a second reply would be typed as input.

var termQueryRE = regexp.MustCompile(
	`\x1b\[6n` + // DSR cursor position
		`|\x1b\[0?c` + // primary device attributes
		`|\x1b\[\?u` + // kitty keyboard protocol flags
		`|\x1b\](1[01]);\?(?:\x07|\x1b\\)` + // OSC 10/11 colour query
		`|\x1b\]4;(\d+);\?(?:\x07|\x1b\\)`) // OSC 4 palette query

// termQueryMaxLen bounds how many trailing bytes can hold a query split across
// two reads; longer than any pattern above.
const termQueryMaxLen = 16

type termQueryTap struct {
	w     io.Writer // where the output goes (unchanged)
	reply io.Writer // the PTY master: answers are typed back to the child
	tail  []byte    // unmatched suffix of the previous write
}

func newTermQueryTap(w, reply io.Writer) io.Writer {
	return &termQueryTap{w: w, reply: reply}
}

func (t *termQueryTap) Write(p []byte) (int, error) {
	buf := append(t.tail, p...)
	end := 0
	for _, m := range termQueryRE.FindAllSubmatchIndex(buf, -1) {
		_, _ = t.reply.Write(termQueryAnswer(buf, m))
		end = m[1]
	}
	// Keep an incomplete escape at the end for the next write.
	rest := buf[end:]
	t.tail = nil
	if i := bytes.LastIndexByte(rest, 0x1b); i >= 0 && len(rest)-i < termQueryMaxLen {
		t.tail = append([]byte(nil), rest[i:]...)
	}
	return t.w.Write(p)
}

func termQueryAnswer(buf []byte, m []int) []byte {
	q := buf[m[0]:m[1]]
	switch {
	case bytes.Equal(q, []byte("\x1b[6n")):
		return []byte("\x1b[1;1R")
	case bytes.HasSuffix(q, []byte("c")) && q[1] == '[':
		return []byte("\x1b[?62;22c")
	case bytes.Equal(q, []byte("\x1b[?u")):
		return []byte("\x1b[?0u")
	case m[2] >= 0: // OSC 10 (fg) / 11 (bg)
		n := string(buf[m[2]:m[3]])
		if n == "10" {
			return []byte("\x1b]10;rgb:ffff/ffff/ffff\x1b\\")
		}
		return []byte("\x1b]11;rgb:0000/0000/0000\x1b\\")
	default: // OSC 4 palette entry
		return []byte("\x1b]4;" + string(buf[m[4]:m[5]]) + ";rgb:8080/8080/8080\x1b\\")
	}
}
