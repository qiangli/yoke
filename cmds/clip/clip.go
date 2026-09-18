// Package clipcmd implements `clip`: read/write the system clipboard
// (pbcopy/pbpaste-style). With no flag it copies its arguments — or stdin — to
// the clipboard; `-o` pastes the clipboard to stdout.
//
// CHARTER EXCEPTION: this is the one coreutils tool that shells out. The OS
// clipboard has no pure-Go path (the cgo alternative is also banned), so it uses
// atotto/clipboard (BSD-3, cgo-free) which execs the platform clipboard utility
// (pbcopy/pbpaste on macOS, xsel/xclip/wl-copy on Linux, powershell on Windows).
// It still cross-compiles everywhere with CGO_ENABLED=0; it fails LOUDLY at
// runtime when the OS clipboard utility is absent (e.g. no xclip on a headless
// Linux box).
package clipcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/atotto/clipboard"

	"github.com/qiangli/coreutils/tool"
)

var cmd = &tool.Tool{
	Name:     "clip",
	Synopsis: "Copy args/stdin to the system clipboard, or -o to paste it to stdout.",
	Usage:    "clip [TEXT...]   (copy);   clip -o   (paste to stdout)",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	out := fs.BoolP("out", "o", false, "paste: read the clipboard to stdout (default: copy stdin/args to the clipboard)")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}

	if *out {
		s, err := clipboard.ReadAll()
		if err != nil {
			fmt.Fprintf(rc.Err, "clip: cannot read clipboard: %v\n", err)
			return 1
		}
		fmt.Fprint(rc.Out, s)
		return 0
	}

	// Copy: explicit args win; otherwise consume stdin (the pbcopy idiom).
	var text string
	if len(operands) > 0 {
		text = strings.Join(operands, " ")
	} else {
		b, err := io.ReadAll(rc.In)
		if err != nil {
			fmt.Fprintf(rc.Err, "clip: %v\n", err)
			return 1
		}
		text = string(b)
	}
	if err := clipboard.WriteAll(text); err != nil {
		fmt.Fprintf(rc.Err, "clip: cannot write clipboard: %v\n", err)
		return 1
	}
	return 0
}
