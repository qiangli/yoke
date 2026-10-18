package msgclients

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
			{Name: "mode", Type: fleet.ArgTypeString, Required: true, Enum: []string{ModePublish, ModeSubscribe}},
			{Name: "topic", Type: fleet.ArgTypeString, Required: true},
			{Name: "payload", Type: fleet.ArgTypeString},
			{Name: "exchange", Type: fleet.ArgTypeString},
			{Name: "qos", Type: fleet.ArgTypeInt},
			{Name: "max-messages", Type: fleet.ArgTypeInt},
			{Name: "max-bytes", Type: fleet.ArgTypeInt},
			{Name: "timeout-seconds", Type: fleet.ArgTypeInt},
			{Name: "explain", Type: fleet.ArgTypeBool},
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
			return "", Op{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("messaging: unsupported argument %q", k), nil)
		}
	}
	client := v["client"]
	c, ok := Lookup(client)
	if !ok {
		return "", Op{}, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("messaging: %q is not admitted (want one of %s)", client, strings.Join(Names(), ", ")), nil)
	}
	_ = c
	num := func(k string) (int64, *gws.Error) {
		if v[k] == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v[k], 10, 64)
		if err != nil || n < 0 {
			return 0, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("messaging: --%s %q is not a non-negative integer", k, v[k]), nil)
		}
		return n, nil
	}
	msgs, e1 := num("max-messages")
	bytes, e2 := num("max-bytes")
	secs, e3 := num("timeout-seconds")
	qos, e4 := num("qos")
	for _, e := range []*gws.Error{e1, e2, e3, e4} {
		if e != nil {
			return "", Op{}, e
		}
	}
	op := Op{
		Mode: v["mode"], Topic: v["topic"], Payload: v["payload"], Exchange: v["exchange"],
		QoS: int(qos), QoSSet: v["qos"] != "",
		MaxMessages: int(msgs), MaxBytes: bytes, Timeout: time.Duration(secs) * time.Second,
	}
	if _, err := op.normalized(); err != nil {
		return "", Op{}, err
	}
	return client, op, nil
}

// publishBin selects the host binary for the mode. mosquitto ships two
// binaries: publish goes to mosquitto_pub, subscribe to mosquitto_sub.
func publishBin(client, mode string) string {
	if client == "mosquitto" {
		if mode == ModePublish {
			return "mosquitto_pub"
		}
		return "mosquitto_sub"
	}
	c, _ := Lookup(client)
	return c.Binaries[0]
}

// Execute is the single entry for CLI and MCP: validate, resolve the
// explicit endpoint, build, then either explain (never executes) or run.
// Output is a loom-v2 envelope on w; the return value is the process exit
// code.
func Execute(ctx context.Context, w io.Writer, getenv func(string) string, locate func(string) (string, error), v Values) int {
	cmdName := "msgclients op"
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
	bin := publishBin(client, op.Mode)
	if !explain {
		p, lerr := locate(bin)
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
