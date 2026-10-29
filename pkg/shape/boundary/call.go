// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package boundary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// EnvVar carries a Policy (JSON) from the host to the interpreter that runs
// the unit. An interpreter that finds it set must install the boundary, and
// must refuse to run when it does not parse (FromEnv).
const EnvVar = "BASHY_APP_BOUNDARY"

// Env returns the EnvVar assignment value for p.
func Env(p Policy) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("boundary: encode policy: %w", err)
	}
	return string(b), nil
}

// FromEnv loads the guard EnvVar names in env (os.Environ shape; nil =
// the process environment). ok is false when no boundary is set; a set but
// malformed one is an error, never "no boundary".
func FromEnv(env []string) (g *Guard, ok bool, err error) {
	var v string
	if env == nil {
		v, ok = os.LookupEnv(EnvVar)
	} else {
		for _, kv := range env {
			if k, val, found := strings.Cut(kv, "="); found && k == EnvVar {
				v, ok = val, true
			}
		}
	}
	if !ok {
		return nil, false, nil
	}
	var p Policy
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return nil, true, fmt.Errorf("boundary: %s: %w", EnvVar, err)
	}
	g, err = New(p)
	return g, true, err
}

// CallRequest is one cross-app call: run the owning app's UNIT as
// Principal, exactly as its own route would.
type CallRequest struct {
	App       string
	Unit      string
	Query     string
	Body      []byte
	Principal string
	From      string
}

// CallResult is the owning app's answer. Status follows HTTP: 200 is the
// unit's output, 404 "no such app or unit", 403 "no access". Neither of the
// latter is an error — a missing item is a normal answer, never a cascade.
type CallResult struct {
	Status int
	Body   []byte
}

// Caller reaches other apps' interfaces.
type Caller interface {
	Call(ctx context.Context, req CallRequest) (CallResult, error)
}

// CallerFunc adapts a function to Caller.
type CallerFunc func(ctx context.Context, req CallRequest) (CallResult, error)

func (f CallerFunc) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	return f(ctx, req)
}

// Endpoint is the host's call broker as the interpreter reaches it: a
// loopback URL and a token bound, by the host, to this run's principal and
// app. The token is the authority; the request's Principal is advisory.
type Endpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// CallTokenHeader carries an Endpoint's token.
const CallTokenHeader = "X-Bashy-App-Call"

// Call implements Caller over HTTP: POST URL/APP/UNIT?QUERY.
func (e *Endpoint) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	u := strings.TrimRight(e.URL, "/") + "/" + url.PathEscape(req.App) + "/" + url.PathEscape(req.Unit)
	if req.Query != "" {
		u += "?" + req.Query
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(req.Body))
	if err != nil {
		return CallResult{}, err
	}
	hr.Header.Set(CallTokenHeader, e.Token)
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return CallResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return CallResult{Status: resp.StatusCode, Body: b}, err
}

type appCall struct {
	app, unit, query string
}

// appCallArgs recognizes `app call APP UNIT [QUERY]`, also spelled through
// bashy itself (`bashy app call ...`).
func appCallArgs(args []string) (appCall, bool) {
	if n := programName(args[0]); (n == "bashy" || n == "bash") && len(args) > 1 {
		args = args[1:]
	}
	if len(args) < 2 || args[0] != "app" || args[1] != "call" {
		return appCall{}, false
	}
	c := appCall{}
	rest := args[2:]
	if len(rest) > 0 {
		c.app = rest[0]
	}
	if len(rest) > 1 {
		c.unit = rest[1]
	}
	if len(rest) > 2 {
		c.query = rest[2]
	}
	if len(rest) > 3 {
		c.app = "" // usage error below
	}
	return c, true
}

const callBodyLimit = 64 << 10

// appCall answers `app call`: stdin is the request body, stdout the owning
// app's answer. Exit 0 on 200; 1 with a one-line reason on not-found, no
// access or an unreachable owner; 2 on usage.
func (g *Guard) appCall(ctx context.Context, hc interp.HandlerContext, c appCall) error {
	if c.app == "" || c.unit == "" {
		fmt.Fprintln(hc.Stderr, "usage: app call APP UNIT [QUERY]")
		return interp.ExitStatus(2)
	}
	if g.caller == nil {
		fmt.Fprintf(hc.Stderr, "app call %s %s: no cross-app interface in this run\n", c.app, c.unit)
		return interp.ExitStatus(1)
	}
	var body []byte
	if hc.Stdin != nil {
		b, err := io.ReadAll(io.LimitReader(hc.Stdin, callBodyLimit+1))
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(hc.Stderr, "app call: read request: %v\n", err)
			return interp.ExitStatus(1)
		}
		if len(b) > callBodyLimit {
			fmt.Fprintln(hc.Stderr, "app call: request body too large")
			return interp.ExitStatus(1)
		}
		body = b
	}
	res, err := g.caller.Call(ctx, CallRequest{App: c.app, Unit: c.unit, Query: c.query, Body: body, Principal: g.p.Principal, From: g.p.App})
	if err != nil {
		fmt.Fprintf(hc.Stderr, "app call %s %s: %v\n", c.app, c.unit, err)
		return interp.ExitStatus(1)
	}
	switch res.Status {
	case http.StatusOK:
		_, _ = hc.Stdout.Write(res.Body)
		return nil
	case http.StatusNotFound:
		fmt.Fprintf(hc.Stderr, "app call %s %s: not found\n", c.app, c.unit)
	case http.StatusForbidden, http.StatusUnauthorized:
		fmt.Fprintf(hc.Stderr, "app call %s %s: no access\n", c.app, c.unit)
	default:
		fmt.Fprintf(hc.Stderr, "app call %s %s: %d %s\n", c.app, c.unit, res.Status, strings.TrimSpace(string(res.Body)))
	}
	return interp.ExitStatus(1)
}
