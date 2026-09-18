// Package hexdumpcmd implements hexdump(1) (util-linux) in its
// canonical -C mode only: hex+ASCII display, 16 bytes per line, with
// the documented default squeezing of repeated identical lines into a
// single "*" and a final line giving the total input offset. Every
// other hexdump format (-b, -c, -d, -o, -x, -e, …) is deliberately
// not supported and fails with the contract error.
//
// Fresh implementation against the util-linux manual (the u-root
// prior art delegates to encoding/hex.Dumper, which has no squeezing
// and no final offset line).
package hexdumpcmd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "hexdump",
	Synopsis: "Display file contents in hexadecimal and ASCII (canonical -C format).\nMultiple files are concatenated. With no FILE, read standard input.",
	Usage:    "hexdump -C [FILE]...",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	canonical := fs.BoolP("canonical", "C", false, "canonical hex+ASCII display")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if !*canonical {
		return tool.NotSupported(rc, cmd, "hexdump formats other than -C/--canonical")
	}

	exit := 0
	var readers []io.Reader
	var closers []io.Closer
	if len(operands) == 0 {
		var in io.Reader = rc.In
		if in == nil {
			in = strings.NewReader("")
		}
		readers = append(readers, in)
	}
	for _, name := range operands {
		f, err := os.Open(rc.Path(name))
		if err != nil {
			fmt.Fprintf(rc.Err, "hexdump: %s: %v\n", name, sysErr(err))
			exit = 1
			continue
		}
		readers = append(readers, f)
		closers = append(closers, f)
	}
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()

	w := bufio.NewWriter(rc.Out)
	if err := dump(io.MultiReader(readers...), w); err != nil {
		fmt.Fprintf(rc.Err, "hexdump: %v\n", err)
		exit = 1
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(rc.Err, "hexdump: write error: %v\n", err)
		return 1
	}
	return exit
}

func dump(r io.Reader, w *bufio.Writer) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var offset int64
	var prev [16]byte
	prevValid := false
	squeezing := false
	block := make([]byte, 16)
	for {
		n, err := io.ReadFull(br, block)
		if n > 0 {
			if n == 16 && prevValid && bytes.Equal(block, prev[:]) {
				if !squeezing {
					if _, werr := w.WriteString("*\n"); werr != nil {
						return werr
					}
					squeezing = true
				}
			} else {
				if werr := writeLine(w, offset, block[:n]); werr != nil {
					return werr
				}
				squeezing = false
				if n == 16 {
					copy(prev[:], block)
					prevValid = true
				} else {
					prevValid = false
				}
			}
			offset += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if offset > 0 {
		if _, err := fmt.Fprintf(w, "%08x\n", offset); err != nil {
			return err
		}
	}
	return nil
}

// writeLine emits one canonical line:
//
//	00000000  68 65 6c 6c 6f 20 77 6f  72 6c 64 0a              |hello world.|
func writeLine(w *bufio.Writer, offset int64, b []byte) error {
	var line []byte
	line = fmt.Appendf(line, "%08x  ", offset)
	for i := 0; i < 16; i++ {
		if i == 8 {
			line = append(line, ' ')
		}
		if i < len(b) {
			line = fmt.Appendf(line, "%02x ", b[i])
		} else {
			line = append(line, "   "...)
		}
	}
	line = append(line, ' ', '|')
	for _, c := range b {
		if c >= 32 && c <= 126 {
			line = append(line, c)
		} else {
			line = append(line, '.')
		}
	}
	line = append(line, '|', '\n')
	_, err := w.Write(line)
	return err
}

func sysErr(err error) error {
	return tool.SysErr(err)
}
