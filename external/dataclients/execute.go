package dataclients

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Schema is the ONE argument surface of the typed operation. CLI binding
// (interp.BindCommandSchema over a registered command) and the MCP schema
// bridge both consume this fleet.CommandSchema and hand the bound values to
// Execute, so validation, bounds and errors cannot diverge between them.
func Schema() *fleet.CommandSchema {
	return &fleet.CommandSchema{
		Positionals: []fleet.CommandParameter{{Name: "client", Type: fleet.ArgTypeString, Required: true, Enum: Names()}},
		Flags: []fleet.CommandFlag{
			{Name: "statement", Type: fleet.ArgTypeString, Required: true},
			{Name: "write", Type: fleet.ArgTypeBool},
			{Name: "commit", Type: fleet.ArgTypeBool},
			{Name: "explain", Type: fleet.ArgTypeBool},
			{Name: "max-rows", Type: fleet.ArgTypeInt},
			{Name: "max-bytes", Type: fleet.ArgTypeInt},
			{Name: "timeout-seconds", Type: fleet.ArgTypeInt},
		},
	}
}

// Values are the bound, string-canonical argument values (as produced by the
// shell binder or the MCP bridge).
type Values map[string]string

// ParseOp validates bound values into a client name and an Op, rejecting
// unknown keys and bad numbers before any provider execution.
func ParseOp(v Values) (string, Op, *gws.Error) {
	known := map[string]bool{"client": true}
	for _, f := range Schema().Flags {
		known[f.Name] = true
	}
	for k := range v {
		if !known[k] {
			return "", Op{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("data client: unsupported argument %q", k), nil)
		}
	}
	client := v["client"]
	if _, ok := Lookup(client); !ok {
		return "", Op{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("data client: %q is not admitted (want one of %s)", client, strings.Join(Names(), ", ")), nil)
	}
	num := func(k string) (int64, *gws.Error) {
		if v[k] == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v[k], 10, 64)
		if err != nil || n < 0 {
			return 0, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("data client: --%s %q is not a non-negative integer", k, v[k]), nil)
		}
		return n, nil
	}
	rows, e1 := num("max-rows")
	bytes, e2 := num("max-bytes")
	secs, e3 := num("timeout-seconds")
	for _, e := range []*gws.Error{e1, e2, e3} {
		if e != nil {
			return "", Op{}, e
		}
	}
	op := Op{Statement: v["statement"], Write: v["write"] == "true", Commit: v["commit"] == "true", MaxRows: int(rows), MaxBytes: bytes, Timeout: time.Duration(secs) * time.Second}
	if _, err := op.normalized(); err != nil {
		return "", Op{}, err
	}
	return client, op, nil
}

// Execute is the single entry for CLI and MCP: validate, resolve the explicit
// endpoint, build, then either explain (never executes) or run. Output is a
// loom-v2 envelope on w; the return value is the process exit code.
func Execute(ctx context.Context, w io.Writer, getenv func(string) string, locate func(string) (string, error), v Values) int {
	cmdName := "dataclients query"
	client, op, err := ParseOp(v)
	if err != nil {
		return err.EmitError(w, cmdName)
	}
	ep, err := ResolveEndpoint(client, getenv)
	if err != nil {
		return err.EmitError(w, cmdName)
	}
	if locate == nil {
		locate = exec.LookPath
	}
	explain := v["explain"] == "true"
	bin := client
	if !explain {
		p, lerr := locate(client)
		if lerr != nil {
			return gws.NewError(gws.CodeUnsupported, HostOnlyError(client).Error(), nil).EmitError(w, cmdName)
		}
		bin = p
	}
	inv, err := Build(ep, op, bin)
	if err != nil {
		return err.EmitError(w, cmdName)
	}
	if explain {
		return weavecli.EmitOK(w, weavecli.OutputJSON, cmdName, inv.Explain(ep))
	}
	res, err := Run(ctx, inv)
	if err != nil {
		return err.EmitError(w, cmdName)
	}
	res.Scope = ep.RedactedScope()
	return Emit(w, cmdName, res)
}
