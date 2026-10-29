// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package boundary is the data boundary a shaped app's unit runs inside,
// enforced by the interpreter rather than by a sandbox, so it is the same
// on every OS.
//
// The rules (operator decision 2026-10-29):
//
//  1. A unit may write only its own app's data for the current principal
//     (BASHY_APP_DATA).
//  2. Another app's data is never touched directly, read or write. Apps
//     interact through the owning app's interface (`app call APP UNIT`)
//     as the same principal; where the owner keeps its data is its own
//     business.
//  3. The owning app manages its own rollback/cleanup through its API.
//  4. Anything outside app data needs an effect the app's Bash# declares
//     (@reads/@writes) AND the user granted at adoption. A candidate can
//     never widen its own grants (Admit).
//  5. A non-bashy external program needs a declared, granted effect
//     (@runs) or a @sandbox() island (which bashy oci runs; this package is
//     then not involved). Redirections and in-process bashy tools are
//     mediated here.
//  6. Other principals' data, BASHY_HOME/bashy state, verifier/evidence and
//     protected paths are never writable — or readable — whatever the grant.
//
// A Guard is built from a Policy. The interpreter installs its
// ExecMiddleware (outside the in-process tool handler) and its
// OpenHandler; the host hands the policy to a child interpreter through
// EnvVar.
package boundary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Op is what a unit is trying to do.
type Op string

const (
	OpRead  Op = "read"
	OpWrite Op = "write"
	OpExec  Op = "exec"
	OpCall  Op = "call"
	OpGrant Op = "grant"
)

// DeniedStatus is the shell status of a command the boundary refused: 126,
// the status the shell yields for a command it will not run (the same one an
// effect-cap denial yields).
const DeniedStatus = 126

// ErrDenied is matched (errors.Is) by every boundary refusal.
var ErrDenied = errors.New("boundary: denied")

// Denied is a boundary refusal with its reason.
type Denied struct {
	Op     Op
	Target string
	Reason string
}

func (d *Denied) Error() string {
	return fmt.Sprintf("boundary: %s %s denied: %s", d.Op, d.Target, d.Reason)
}

func (d *Denied) Is(t error) bool { return t == ErrDenied }

// Grants are the effects outside app data a unit may have: what its Bash#
// declares, narrowed to what the user granted at adoption.
type Grants struct {
	Read     []string `json:"read,omitempty"`
	Write    []string `json:"write,omitempty"`
	Programs []string `json:"programs,omitempty"`
}

// Empty reports whether g grants nothing.
func (g Grants) Empty() bool {
	return len(g.Read) == 0 && len(g.Write) == 0 && len(g.Programs) == 0
}

// Policy is one unit run's boundary. Paths are absolute host paths.
type Policy struct {
	App       string `json:"app"`
	Principal string `json:"principal,omitempty"`
	// Data is the app's private data for this principal (BASHY_APP_DATA):
	// the only location writable without a grant.
	Data string `json:"data"`
	// Home is the bashy home. Everything under it except Data is out of
	// reach: other principals' and apps' data, bashy state, evidence.
	Home string `json:"home,omitempty"`
	// Protected are further never-reachable roots (the verifier, the
	// shaping work root, ...).
	Protected []string `json:"protected,omitempty"`
	// Grants are the effective, already-admitted grants (see Admit).
	Grants Grants `json:"grants,omitempty"`
	// Call is where `app call` reaches other apps' interfaces; nil = no
	// cross-app calls from this run.
	Call *Endpoint `json:"call,omitempty"`
}

// Guard enforces one Policy.
type Guard struct {
	p         Policy
	data      string
	home      string
	protected []string
	read      []string
	write     []string
	caller    Caller
}

// New validates p and returns its guard.
func New(p Policy) (*Guard, error) {
	if strings.TrimSpace(p.Data) == "" || !filepath.IsAbs(p.Data) {
		return nil, errors.New("boundary: policy needs an absolute app data directory")
	}
	g := &Guard{p: p, data: resolve(p.Data)}
	if p.Home != "" {
		g.home = resolve(p.Home)
	}
	for _, r := range p.Protected {
		if filepath.IsAbs(r) {
			g.protected = append(g.protected, resolve(r))
		}
	}
	for _, r := range p.Grants.Read {
		if filepath.IsAbs(r) {
			g.read = append(g.read, resolve(r))
		}
	}
	for _, r := range p.Grants.Write {
		if filepath.IsAbs(r) {
			g.write = append(g.write, resolve(r))
		}
	}
	if p.Call != nil {
		g.caller = p.Call
	}
	return g, nil
}

// WithCaller replaces the cross-app call transport (an in-process host, a
// test). It returns g.
func (g *Guard) WithCaller(c Caller) *Guard {
	g.caller = c
	return g
}

// Policy returns the policy g enforces.
func (g *Guard) Policy() Policy { return g.p }

// Check decides one file access. path is absolute or relative to dir.
func (g *Guard) Check(op Op, dir, path string) error {
	if isDevice(path) {
		return nil
	}
	if isNetDevice(path) {
		return &Denied{Op: op, Target: path, Reason: "network redirection is not an app data effect"}
	}
	target := path
	if !filepath.IsAbs(target) && !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, `\`) {
		target = filepath.Join(dir, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return &Denied{Op: op, Target: path, Reason: "unresolvable path"}
	}
	t := resolve(abs)
	if within(g.data, t) {
		return nil
	}
	if reason := g.unreachable(t); reason != "" {
		return &Denied{Op: op, Target: path, Reason: reason}
	}
	for _, r := range g.write {
		if within(r, t) {
			return nil
		}
	}
	if op == OpRead {
		for _, r := range g.read {
			if within(r, t) {
				return nil
			}
		}
	}
	return &Denied{Op: op, Target: path, Reason: "outside the app's own data and its granted locations"}
}

// unreachable names why t is out of reach whatever the grant, or "".
func (g *Guard) unreachable(t string) string {
	for _, r := range g.protected {
		if within(r, t) {
			return "protected path"
		}
	}
	if g.home == "" || !within(g.home, t) {
		return ""
	}
	shape := filepath.Join(g.home, "shape")
	// Data is <home>/shape/<app>/<principal>/<data or preview/ID>.
	if rel, err := filepath.Rel(shape, g.data); within(shape, t) && err == nil {
		if seg := strings.Split(rel, string(filepath.Separator)); len(seg) >= 3 {
			app := filepath.Join(shape, seg[0])
			own := filepath.Join(app, seg[1])
			switch {
			case within(own, t):
				return "the app's own state is bashy's, reached only through its API"
			case within(app, t):
				return "another principal's data"
			default:
				return "another app's data: use its interface (app call) instead"
			}
		}
	}
	return "bashy state (BASHY_HOME)"
}

// CheckProgram decides running a non-bashy external program by name.
func (g *Guard) CheckProgram(name string) error {
	n := programName(name)
	for _, p := range g.p.Grants.Programs {
		if programName(p) == n {
			return nil
		}
	}
	return &Denied{Op: OpExec, Target: name, Reason: "undeclared external program (declare @runs and have it granted, or ask for @sandbox())"}
}

// Admit narrows what a unit declares to what the user granted at adoption.
// Any declared effect the grant does not cover refuses: the candidate is
// trying to widen its own grants. The result is the declaration itself —
// least privilege, never the whole grant.
func Admit(declared, granted Grants) (Grants, error) {
	for _, r := range declared.Read {
		if !covered(r, granted.Read) && !covered(r, granted.Write) {
			return Grants{}, &Denied{Op: OpGrant, Target: "read " + r, Reason: "declared effect not granted at adoption (a candidate cannot widen its own grants)"}
		}
	}
	for _, r := range declared.Write {
		if !covered(r, granted.Write) {
			return Grants{}, &Denied{Op: OpGrant, Target: "write " + r, Reason: "declared effect not granted at adoption (a candidate cannot widen its own grants)"}
		}
	}
	for _, p := range declared.Programs {
		if !slices.ContainsFunc(granted.Programs, func(q string) bool { return programName(q) == programName(p) }) {
			return Grants{}, &Denied{Op: OpGrant, Target: "run " + p, Reason: "declared program not granted at adoption (a candidate cannot widen its own grants)"}
		}
	}
	return declared, nil
}

func covered(p string, roots []string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rp := resolve(p)
	for _, r := range roots {
		if filepath.IsAbs(r) && within(resolve(r), rp) {
			return true
		}
	}
	return false
}

// ExpandHome turns a leading ~ into home in each path; relative paths are
// dropped (a grant names a place, never "wherever the unit is").
func ExpandHome(paths []string, home string) []string {
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "~" {
			p = home
		} else if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, filepath.FromSlash(rest))
		}
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

func programName(n string) string {
	n = strings.ToLower(filepath.Base(strings.ReplaceAll(n, `\`, "/")))
	return strings.TrimSuffix(n, ".exe")
}

// isDevice reports the stream pseudo-files every unit may use.
func isDevice(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdin", "/dev/stdout", "/dev/stderr":
		return true
	}
	if p == os.DevNull || (runtime.GOOS == "windows" && strings.EqualFold(p, "NUL")) {
		return true
	}
	return strings.HasPrefix(p, "/dev/fd/")
}

func isNetDevice(p string) bool {
	return strings.HasPrefix(p, "/dev/tcp/") || strings.HasPrefix(p, "/dev/udp/")
}

// within reports whether p is root or beneath it. Both are resolved.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// resolve cleans p and resolves symlinks along its longest existing prefix,
// so a link inside app data cannot point a write somewhere else.
func resolve(p string) string {
	p = filepath.Clean(p)
	cur, rest := p, []string(nil)
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}
