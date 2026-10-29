// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package boundary

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/coreutils/shell"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

// ExecMiddleware is the boundary on every command the interpreter
// dispatches. It must sit OUTSIDE the in-process tool handler (so it sees
// the tools that handler serves without calling next) and is the one place
// `app call` is answered:
//
//   - `app call APP UNIT [QUERY]` reaches another app's interface as this
//     run's principal (see Caller);
//   - an in-process tool that reads or writes has its working directory and
//     every path operand checked (writers need write access to all of
//     them — conservative, never permissive);
//   - an in-process tool that spawns a program (env, xargs, timeout, ...)
//     and every command that is not an in-process tool is an external
//     program: refused unless declared and granted.
func (g *Guard) ExecMiddleware() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			hc := interp.HandlerCtx(ctx)
			if call, ok := appCallArgs(args); ok {
				return g.appCall(ctx, hc, call)
			}
			if err := g.checkCommand(hc.Dir, args); err != nil {
				fmt.Fprintln(hc.Stderr, err)
				return interp.ExitStatus(DeniedStatus)
			}
			return next(ctx, args)
		}
	}
}

// checkCommand decides one dispatched argv.
func (g *Guard) checkCommand(dir string, args []string) error {
	name := programName(args[0])
	if tool.Lookup(args[0]) == nil {
		return g.CheckProgram(args[0])
	}
	e, _ := atlas.Lookup(name)
	effects := e.Effects
	if slices.Contains(effects, atlas.EffExec) {
		if err := g.CheckProgram(name); err != nil {
			return err
		}
	}
	op := Op("")
	for _, eff := range effects {
		switch eff {
		case atlas.EffWrite, "destroy", atlas.EffPersist:
			op = OpWrite
		case atlas.EffRead:
			if op == "" {
				op = OpRead
			}
		}
	}
	if op == "" || noPathOperands[name] {
		return nil
	}
	if err := g.Check(op, dir, dir); err != nil {
		return err
	}
	for _, a := range pathOperands(name, dir, args[1:]) {
		if err := g.Check(op, dir, a); err != nil {
			return err
		}
	}
	return nil
}

// noPathOperands are read-classified tools whose operands are never files.
var noPathOperands = map[string]bool{
	"tr": true, "date": true, "expr": true, "basename": true, "dirname": true,
	"sleep": true, "printenv": true, "id": true, "whoami": true, "uname": true,
	"hostname": true, "true": true, "false": true, "test": true, "[": true,
}

// scriptFirst are tools whose first operand is a pattern or program unless
// one is given by flag.
var scriptFirst = map[string][]string{
	"grep": {"-e", "-f", "--regexp", "--file"}, "egrep": {"-e", "-f"}, "fgrep": {"-e", "-f"},
	"sed": {"-e", "-f", "--expression", "--file"}, "awk": {"-f", "--file"}, "jq": {"-f", "--from-file"},
}

// pathOperands returns the operands that may name a file: flag values after
// '=', and every non-flag operand that is absolute, has a separator or a
// '..', or exists beneath dir (a plain name is dir-relative, and dir itself
// is checked, but an existing one may be a symlink out).
func pathOperands(name, dir string, args []string) []string {
	var out []string
	skipFirst := false
	if flags, ok := scriptFirst[name]; ok {
		skipFirst = !slices.ContainsFunc(args, func(a string) bool {
			for _, f := range flags {
				if a == f || strings.HasPrefix(a, f+"=") || (len(f) == 2 && strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--")) {
					return true
				}
			}
			return false
		})
	}
	operands := true
	for _, a := range args {
		if operands && a == "--" {
			operands = false
			continue
		}
		if operands && strings.HasPrefix(a, "-") && a != "-" {
			if _, v, ok := strings.Cut(a, "="); ok && looksLikePath(dir, v) {
				out = append(out, v)
			}
			continue
		}
		if skipFirst {
			skipFirst = false
			continue
		}
		if a != "-" && looksLikePath(dir, a) {
			out = append(out, a)
		}
	}
	return out
}

func looksLikePath(dir, a string) bool {
	if a == "" {
		return false
	}
	if filepath.IsAbs(a) || strings.ContainsAny(a, `/\`) || filepath.VolumeName(a) != "" {
		return true
	}
	_, err := os.Lstat(filepath.Join(dir, a))
	return err == nil
}

// OpenHandler wraps next (nil = interp.DefaultOpenHandler()) so every
// redirection and builtin open is checked: a write-flagged open needs write
// access, any other open read access.
func (g *Guard) OpenHandler(next interp.OpenHandlerFunc) interp.OpenHandlerFunc {
	if next == nil {
		next = interp.DefaultOpenHandler()
	}
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		op := OpRead
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
			op = OpWrite
		}
		if err := g.Check(op, interp.HandlerCtx(ctx).Dir, path); err != nil {
			// A *PathError fails the one command (status 1, message on
			// stderr) instead of halting the runner.
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return next(ctx, path, flag, perm)
	}
}

// RunnerOptions are the interpreter options that install the boundary on
// a runner that has no other open handler or exec middleware: the open
// handler plus the exec middleware followed by the in-process tools. A
// host with its own chain wires OpenHandler and ExecMiddleware itself,
// keeping ExecMiddleware outside its tool handler.
func (g *Guard) RunnerOptions(tools func(interp.ExecHandlerFunc) interp.ExecHandlerFunc) []interp.RunnerOption {
	mws := []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{g.ExecMiddleware()}
	if tools != nil {
		mws = append(mws, tools)
	}
	return []interp.RunnerOption{interp.OpenHandler(g.OpenHandler(nil)), interp.ExecHandlers(mws...)}
}

// Decorators are the boundary's declaration decorators — @reads, @writes,
// @runs — as the interpreter sees them: run policy the HOST already read
// (unit.ReadContract) and admitted before the run, so at run time each one
// simply calls through. An interpreter merges them into its own set.
func Decorators() map[string]interp.DecoratorFunc {
	through := func(ctx context.Context, c *interp.Call, _ []interp.DecoratorArg) error {
		c.Next(ctx)
		return nil
	}
	return map[string]interp.DecoratorFunc{"reads": through, "writes": through, "runs": through}
}

// Run interprets script under g with bashy's pure-Go tools in-process: the
// reference wiring for an interpreter that found EnvVar set (the tools
// are whatever the binary registered, e.g. coreutils' cmds/all). dir is the
// working directory (the unit's app data), env the full environment.
func Run(ctx context.Context, g *Guard, script, name, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(script), name)
	if err != nil {
		return 2, err
	}
	opts := append(g.RunnerOptions(shell.Handler()), interp.Decorators(Decorators()),
		interp.Dir(dir), interp.Env(expand.ListEnviron(env...)), interp.StdIO(stdin, stdout, stderr))
	r, err := interp.New(opts...)
	if err != nil {
		return 2, err
	}
	if err := r.Run(ctx, f); err != nil {
		if st, ok := interp.IsExitStatus(err); ok {
			return int(st), nil
		}
		return 1, err
	}
	return 0, nil
}
