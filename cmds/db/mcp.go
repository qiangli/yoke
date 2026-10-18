package dbcmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/mcp"
	"github.com/qiangli/yoke/pkg/atlas"
)

// MCPCommand exposes the same declared flags as a typed direct MCP tool.
// The generic run_tool remains available and uses the same registered runner.
func MCPCommand() mcp.RegisteredCommand {
	schema := Schema()
	entry, _ := atlas.Lookup("db")
	return mcp.RegisteredCommand{
		Name: "db_lifecycle", Synopsis: command.Synopsis, Usage: command.Usage,
		Schema: &schema, Effects: append([]string(nil), entry.Effects...),
		Run: func(ctx context.Context, argv []string, stdin, dir string) (string, string, int) {
			ordered, err := mcpArgv(schema, argv)
			if err != nil {
				return "", err.Error(), 2
			}
			var out, stderr bytes.Buffer
			rc := &tool.RunContext{Ctx: ctx, Dir: dir, Env: os.Environ(), In: strings.NewReader(stdin), Out: &out, Err: &stderr}
			code := tool.Lookup("db").Run(rc, ordered)
			return out.String(), stderr.String(), code
		},
	}
}

// ArgSchema emits flags first. The multicall command uses its verb first.
func mcpArgv(schema tool.ArgSchema, argv []string) ([]string, error) {
	flagTypes := make(map[string]string, len(schema.Flags))
	for _, flag := range schema.Flags {
		flagTypes["--"+flag.Name] = flag.Type
	}
	i := 0
	for i < len(argv) && strings.HasPrefix(argv[i], "--") {
		typ, ok := flagTypes[argv[i]]
		if !ok {
			return nil, fmt.Errorf("unknown flag %q", argv[i])
		}
		i++
		if typ != "bool" {
			if i >= len(argv) {
				return nil, fmt.Errorf("missing value for flag")
			}
			i++
		}
	}
	if i >= len(argv) {
		return nil, fmt.Errorf("missing db verb")
	}
	out := []string{argv[i]}
	out = append(out, argv[:i]...)
	out = append(out, argv[i+1:]...)
	return out, nil
}
