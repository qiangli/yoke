// Package mcp exposes the coreutils tool registry over the Model Context
// Protocol — the third consumption surface (alongside the in-process
// shell ExecHandler and the busybox multicall binary) so that non-Go
// agents (codex, claude, …) can drive the AgentOS userland.
//
// The compatibility list_tools and run_tool tools cover the complete registry.
// Options can additionally expose selected commands, registered commands with
// typed schemas, and an optional bashy script runner as individual tools.
// Execution is gated by the server's effect policy.
//
// This package uses the official SDK (github.com/modelcontextprotocol/go-sdk),
// the same one the umbrella already pins, aliased mcpsdk to avoid clashing
// with this package's own name.
package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/coreutils/multicall"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

// Options configures policy and direct command exposure. With neither Tools nor
// AllTools set, the registry is exposed only through the compatibility tools.
// Registered and RunScript independently add caller-owned tools.
type Options struct {
	Policy     *Policy
	Tools      []string // Explicit registry names; unknown names are errors.
	AllTools   bool     // Add canonical registry names supported on runtime.GOOS.
	Registered func() []RegisteredCommand
	RunScript  func(ctx context.Context, script, stdin, dir string) (stdout, stderr string, exit int, err error)
}

// ToolInfo describes one registered tool for list_tools. Group and Caps are
// the Command Atlas axes (pkg/atlas): the functional group and the agentic
// capability flags; both are additive and omitted when the atlas has no
// entry for the name.
type ToolInfo struct {
	Name     string   `json:"name" jsonschema:"command name as spelled on the CLI"`
	Synopsis string   `json:"synopsis" jsonschema:"one-line description"`
	Usage    string   `json:"usage" jsonschema:"usage line"`
	Group    string   `json:"group,omitempty" jsonschema:"Command Atlas functional group (fileutils, textutils, code-intel, …)"`
	Caps     []string `json:"caps,omitempty" jsonschema:"Command Atlas capability flags (json, read-only, destructive, …)"`
}

// ListToolsInput is empty — list_tools takes no arguments.
type ListToolsInput struct{}

// ListToolsOutput is the list_tools result.
type ListToolsOutput struct {
	Tools []ToolInfo `json:"tools" jsonschema:"every tool this build ships, sorted by name"`
}

// RunToolInput is the run_tool argument set.
type RunToolInput struct {
	Name  string            `json:"name" jsonschema:"tool to run, e.g. \"grep\""`
	Args  []string          `json:"args,omitempty" jsonschema:"argument vector after the tool name"`
	Stdin string            `json:"stdin,omitempty" jsonschema:"standard input fed to the tool"`
	Dir   string            `json:"dir,omitempty" jsonschema:"working directory; relative operands resolve against it"`
	Env   map[string]string `json:"env,omitempty" jsonschema:"environment for the invocation; empty means none"`
}

// RunToolOutput is the run_tool result.
type RunToolOutput struct {
	Stdout   string `json:"stdout" jsonschema:"captured standard output"`
	Stderr   string `json:"stderr" jsonschema:"captured standard error"`
	ExitCode int    `json:"exit_code" jsonschema:"process exit status (0 success, 2 usage error, …)"`
}

// NewServer builds an MCP server exposing the current tool registry via
// the generic list_tools / run_tool pair. Callers must have blank-imported
// the tool sets they want available (e.g. coreutils/cmds/all) before any
// client lists or runs tools.
func NewServer(name, version string) *mcpsdk.Server {
	return NewServerWithOptions(name, version, Options{})
}

// NewServerWithOptions builds a server with an explicit effect policy.
// It panics on invalid tool selection; use RegisterDirectTools for error handling.
func NewServerWithOptions(name, version string, opts Options) *mcpsdk.Server {
	srv, err := BuildServer(name, version, opts)
	if err != nil {
		panic(err)
	}
	return srv
}

// BuildServer is NewServerWithOptions returning the direct-tool registration
// error (an unknown name in Options.Tools, an invalid registered schema)
// instead of panicking — the form a command-line front door wants.
func BuildServer(name, version string, opts Options) (*mcpsdk.Server, error) {
	policy := opts.Policy
	if policy == nil {
		policy = &Policy{}
	}
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: name, Version: version},
		&mcpsdk.ServerOptions{
			// MCP 2026-07-28 deprecates logging/roots/sampling; an explicit empty
			// capability set stops the SDK advertising logging by default while
			// tools stay inferred from the registered tool set.
			Capabilities: &mcpsdk.ServerCapabilities{},
			Instructions: "Pure-Go AgentOS userland. Use list_tools to discover commands, run_tool to execute one. Tools follow GNU semantics for the flags they implement and fail loudly (exit 2) on unsupported flags rather than guessing.",
		},
	)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "list_tools",
		Description: "List the pure-Go userland commands this build ships, with a one-line synopsis each.",
	}, listToolsHandler)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "run_tool",
		Description: "Run one userland command. Provide name + args (and optional stdin/dir/env); returns stdout, stderr, and exit_code. No process is spawned — execution is in-process and pure Go.",
	}, policy.runToolHandler)

	addServerInfo(srv, name, version, policy)
	directStateFor(srv, policy)
	if err := RegisterDirectTools(srv, opts); err != nil {
		return nil, err
	}

	return srv, nil
}

// ServeStdio runs an MCP server over stdio until the transport closes or
// ctx is cancelled — the entrypoint a `coreutils mcp` / `bashy mcp` front
// end calls. A client closing stdin (io.EOF) is the normal way to end a
// stdio session, so it is reported as a clean shutdown (nil), not an error.
func ServeStdio(ctx context.Context, name, version string) error {
	err := NewServer(name, version).Run(ctx, &mcpsdk.StdioTransport{})
	if isCleanShutdown(err) {
		return nil
	}
	return err
}

// isCleanShutdown reports whether err is the benign end-of-session signal a
// stdio server sees when the client closes the pipe. The SDK reports this as
// io.EOF or its internal jsonrpc2 "server is closing" sentinel (an internal
// package, so matched by message, not type).
func isCleanShutdown(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	return strings.Contains(err.Error(), "server is closing")
}

func listToolsHandler(_ context.Context, _ *mcpsdk.CallToolRequest, _ ListToolsInput) (*mcpsdk.CallToolResult, ListToolsOutput, error) {
	names := tool.Names()
	infos := make([]ToolInfo, 0, len(names))
	for _, n := range names {
		t := tool.Lookup(n)
		if t == nil {
			continue
		}
		info := ToolInfo{Name: t.Name, Synopsis: t.Synopsis, Usage: t.Usage}
		if e, ok := atlas.Lookup(t.Name); ok {
			info.Group, info.Caps = e.Group, e.Caps
		}
		infos = append(infos, info)
	}
	return nil, ListToolsOutput{Tools: infos}, nil
}

func (p *Policy) runToolHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in RunToolInput) (*mcpsdk.CallToolResult, RunToolOutput, error) {
	if err := p.Check(in.Name, (*Policy)(nil).commandEffects(in.Name)); err != nil {
		res, out := p.policyDenial(in.Name, err)
		return res, out, nil
	}
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx: ctx,
		Dir: in.Dir,
		Env: envSlice(in.Env),
		FS:  tool.NewLocalFS(),
		Stdio: tool.Stdio{
			In:  strings.NewReader(in.Stdin),
			Out: &out,
			Err: &errb,
		},
	}
	code := multicall.Dispatch(rc, in.Name, in.Args)
	return nil, RunToolOutput{
		Stdout:   out.String(),
		Stderr:   errb.String(),
		ExitCode: code,
	}, nil
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
