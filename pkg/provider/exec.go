package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// maxResultBytes bounds what an executable adapter may write to stdout.
const maxResultBytes = 16 << 20

// ExecOptions configures the executable-plugin runner.
type ExecOptions struct {
	// Stderr receives the plugin's diagnostics; nil discards them.
	Stderr io.Writer
	// Env is the plugin's environment; nil inherits the caller's.
	Env []string
	// Dir is the plugin's working directory; empty inherits the caller's.
	Dir string
}

// NewExecAdapter returns the runner for an executable provider plugin.
// Each Do execs m.Argv directly (never through a shell), writes the
// Request as one JSON document on stdin, and reads one Result from stdout;
// stderr carries diagnostics only. The runner is wrapped in Guard, so an
// undeclared operation, a missing idempotency key or an unauthorized
// destroy is refused without executing the plugin.
func NewExecAdapter(m Manifest, opts ExecOptions) (Adapter, error) {
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	if m.Form != FormExecutable {
		return nil, &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("manifest %s: form %q is not executable", m.Name, m.Form)}
	}
	return Guard(&execAdapter{m: m, opts: opts}), nil
}

type execAdapter struct {
	m    Manifest
	opts ExecOptions
}

func (x *execAdapter) Manifest() Manifest { return x.m }

func (x *execAdapter) Do(ctx context.Context, req Request) (Result, error) {
	in, err := json.Marshal(req)
	if err != nil {
		return Failure(req.Op, ErrInvalidRequest, "encode request: %v", err)
	}
	cmd := exec.CommandContext(ctx, x.m.Argv[0], x.m.Argv[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = x.opts.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	cmd.Env = x.opts.Env
	cmd.Dir = x.opts.Dir
	runErr := cmd.Run()
	if ctx.Err() != nil {
		e := &Error{Code: ErrTimeout, Message: fmt.Sprintf("plugin %s: %v", x.m.Name, ctx.Err()), Retryable: true}
		return Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e}, e
	}
	if out.overflow {
		return Failure(req.Op, ErrProvider, "plugin %s: result exceeds %d bytes", x.m.Name, maxResultBytes)
	}
	var r Result
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	if err := dec.Decode(&r); err != nil {
		if runErr != nil {
			return Failure(req.Op, ErrProvider, "plugin %s failed without a result: %v", x.m.Name, runErr)
		}
		return Failure(req.Op, ErrProvider, "plugin %s: invalid result: %v", x.m.Name, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Failure(req.Op, ErrProvider, "plugin %s: trailing data after the result", x.m.Name)
	}
	if r.ProtocolVersion != ProtocolVersion {
		return Failure(req.Op, ErrProtocolMismatch, "plugin %s answered protocol %q, want %q", x.m.Name, r.ProtocolVersion, ProtocolVersion)
	}
	if r.Error == nil && runErr != nil {
		return Failure(req.Op, ErrProvider, "plugin %s exited with %v after a success result", x.m.Name, runErr)
	}
	return r, r.Err()
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxResultBytes {
		b.overflow = true
		return 0, errors.New("result too large")
	}
	return b.Buffer.Write(p)
}

// Serve is the plugin side of the executable protocol: it reads one
// Request from r, runs it on a, and writes one Result to w. A plugin's main
// is typically `provider.Serve(ctx, adapter, os.Stdin, os.Stdout)` with
// diagnostics on os.Stderr. A normalized failure is a Result, not a Serve
// error; Serve fails only when it cannot read the request or write the
// result.
func Serve(ctx context.Context, a Adapter, r io.Reader, w io.Writer) error {
	var req Request
	dec := json.NewDecoder(r)
	var res Result
	if err := dec.Decode(&req); err != nil {
		res, _ = Failure("", ErrInvalidRequest, "decode request: %v", err)
	} else {
		res, _ = Guard(a).Do(ctx, req)
	}
	if res.ProtocolVersion == "" {
		res.ProtocolVersion = ProtocolVersion
	}
	return json.NewEncoder(w).Encode(res)
}
