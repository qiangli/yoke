// yoke is the busybox-style multicall binary over the WHOLE bashy userland:
// the certified required set from github.com/qiangli/coreutils plus every
// yoke applet. Invoke a tool as `yoke <name> [args...]`, or symlink/rename the
// binary to a tool name and invoke it directly (argv[0] dispatch).
//
// The reserved front-end subcommand `yoke mcp` starts the Model Context
// Protocol server over stdio instead of dispatching a tool. (It used to be
// `coreutils mcp`; the certified coreutils binary is multicall-only now.)
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/coreutils/multicall"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"

	_ "github.com/qiangli/yoke/cmds/all"
)

func main() {
	// Only the `yoke mcp` front-end form starts the server; when the binary
	// is symlinked to a tool name, `mcp` is just that tool's operand.
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if base == "yoke" && len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := mcp.ServeStdio(context.Background(), "yoke", tool.Version); err != nil {
			fmt.Fprintln(os.Stderr, "yoke mcp:", err)
			os.Exit(1)
		}
		return
	}
	multicall.Main("yoke")
}
