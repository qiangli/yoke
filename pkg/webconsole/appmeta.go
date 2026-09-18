// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/atlas"
)

// MetaSchema identifies the self-description contract. It is spoken by three
// surfaces, deliberately: `<bin> meta --json` (a third-party app), `bashy
// <verb> meta` (a bashy verb, out of the atlas), and `GET /meta/<app>` (the
// console, for a consumer that cannot exec). One schema, so the console's own
// panels and a foreign program are described by the same bytes.
const MetaSchema = "dhnt-app-meta-v1"

// Auth tiers a panel can declare.
const (
	// AuthPublic admits anyone who can reach the console, with no identity.
	// It opens THAT MOUNT ONLY — never /api, never another panel.
	AuthPublic = "public"
	// AuthSystem is the console's own ladder: cloud vouch, else a console
	// session cookie, else ungated loopback. The default.
	AuthSystem = "system"
	// AuthCustom admits unauthenticated and lets the app's own login page
	// answer. The console adds no gate and swallows no redirect.
	AuthCustom = "custom"
)

// ErrNotAnApp means the binary ran but does not speak this contract — no JSON,
// or the wrong schema_version.
//
// `meta` is a common word. A binary with an unrelated `meta` subcommand will
// answer something that is not this schema, and one with no such subcommand may
// still exit 0 printing a usage banner. So the probe demands a positive
// identification and never half-parses: a wrong answer is the same as no answer,
// and the caller falls back to the binary's basename.
var ErrNotAnApp = errors.New("does not speak " + MetaSchema)

// metaProbeTimeout bounds the exec. A program that cannot describe itself in
// three seconds is not one the launcher should block its start page on.
var metaProbeTimeout = 3 * time.Second

// metaProbeLimit caps what we read from a probed binary's stdout.
const metaProbeLimit = 64 << 10

const (
	metaNameLimit  = 64
	metaLabelLimit = 128
	metaTipLimit   = 512
	metaStartLimit = 4 << 10
)

var errMetaOutputTooLarge = errors.New("meta output exceeds 64 KiB")

// AppMeta is one self-description. Everything but the transport facts is
// optional: a program that answers nothing but a port still gets a usable tile.
type AppMeta struct {
	SchemaVersion string `json:"schema_version"`

	Name  string `json:"name,omitempty"`  // default: binary basename
	Label string `json:"label,omitempty"` // default: Name
	Icon  string `json:"icon,omitempty"`  // SVG path data on a 24 grid, or one emoji
	Tip   string `json:"tip,omitempty"`   // the title= tooltip

	Mount string   `json:"mount,omitempty"` // default: Name. ONE path segment
	Mode  string   `json:"mode,omitempty"`  // proxy — the only mode a third party can be
	Port  int      `json:"port,omitempty"`
	Start []string `json:"start,omitempty"` // complete argv for the stopped-tile start hint

	Auth      string `json:"auth,omitempty"`       // request only; operator --app-auth is authoritative
	LoginPath string `json:"login_path,omitempty"` // custom only, app-relative
}

// consoleReserved are mounts the console itself owns. This is deliberately NOT
// merged into atlas.ReservedMounts(), which is a mirror of outpost's and
// cloudbox's name lists and must not grow a bashy-only entry.
//
// `login` and `shell` are unprotected today because no third party could
// declare a mount at all; admitting one without this would let an app claim
// `login` and shadow the console's own sign-in page.
var consoleReserved = map[string]bool{
	"meta": true, "login": true, "shell": true, "term": true,
}

// iconPath accepts only what is safe to interpolate into an SVG `d=` attribute.
// An icon is attacker-adjacent input — it comes from a binary the operator
// named, but a compromised or careless one must not be able to close the
// attribute and inject markup.
var iconPath = regexp.MustCompile(`^[MmLlHhVvCcSsQqTtAaZz0-9eE.,+\-\s]+$`)

// ParseAppSpec splits a --app value: `<bin>` or `<bin>@<port>`.
//
// The port is the fallback rung: a binary with no `meta` subcommand still gets
// a tile when the operator supplies the one fact the console cannot guess.
func ParseAppSpec(spec string) (bin string, port int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", 0, errors.New("empty --app")
	}
	// Split on the LAST '@' so a path containing one still works.
	if i := strings.LastIndex(spec, "@"); i > 0 {
		p, perr := strconv.Atoi(spec[i+1:])
		if perr != nil {
			return "", 0, fmt.Errorf("%q: port after '@' is not a number", spec)
		}
		if p < 1 || p > 65535 {
			return "", 0, fmt.Errorf("%q: port %d out of range", spec, p)
		}
		return spec[:i], p, nil
	}
	return spec, 0, nil
}

// ProbeFunc is the exec seam. Tests inject one so a unit test never spawns a
// process; the console defaults to ProbeApp.
type ProbeFunc func(ctx context.Context, bin string) (AppMeta, error)

// ProbeApp runs `<bin> meta --json` and parses the result.
//
// It reads stdout ONLY. bashy prints `bashy: telemetry on → …` to stderr, and
// merging the two streams is exactly the defect that has been silently breaking
// every `weave doctor` probe in pkg/board/sources.go: CombinedOutput() prepends
// the banner and the JSON never parses. Any program under a telemetry-emitting
// harness has the same shape, so this must stay Output().
func ProbeApp(ctx context.Context, bin string) (AppMeta, error) {
	path, err := resolveBin(bin)
	if err != nil {
		return AppMeta{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, metaProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "meta", "--json")
	cmd.Stdin = nil
	cmd.WaitDelay = 500 * time.Millisecond
	out := &cappedMetaOutput{limit: metaProbeLimit}
	cmd.Stdout = out // stdout only — see the doc comment
	err = cmd.Run()
	if out.tooLarge {
		return AppMeta{}, fmt.Errorf("%s meta: %w", bin, errMetaOutputTooLarge)
	}
	if err != nil {
		if ctx.Err() != nil {
			return AppMeta{}, fmt.Errorf("%s meta: timed out after %s", bin, metaProbeTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return AppMeta{}, fmt.Errorf("%s meta: %w", bin, ErrNotAnApp)
		}
		return AppMeta{}, fmt.Errorf("%s meta: %w", bin, err)
	}
	return ParseMeta(out.Bytes())
}

// cappedMetaOutput bounds memory while the child is still writing. Returning
// an error closes os/exec's reader side of the pipe, so a producer cannot block
// us by continuing forever after the cap binds.
type cappedMetaOutput struct {
	buf      bytes.Buffer
	limit    int
	tooLarge bool
}

func (w *cappedMetaOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit + 1 - w.buf.Len()
	if remaining > 0 {
		if remaining < len(p) {
			p = p[:remaining]
		}
		_, _ = w.buf.Write(p)
	}
	if w.buf.Len() > w.limit {
		w.tooLarge = true
		return n, errMetaOutputTooLarge
	}
	return n, nil
}

func (w *cappedMetaOutput) Bytes() []byte { return w.buf.Bytes() }

// ParseMeta decodes a probe result, demanding a positive identification.
func ParseMeta(b []byte) (AppMeta, error) {
	var m AppMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return AppMeta{}, ErrNotAnApp
	}
	if m.SchemaVersion != MetaSchema {
		return AppMeta{}, ErrNotAnApp
	}
	return m, nil
}

// resolveBin accepts an absolute or relative path, else looks up PATH.
func resolveBin(bin string) (string, error) {
	if strings.ContainsRune(bin, filepath.Separator) || strings.ContainsRune(bin, '/') {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return "", fmt.Errorf("%s: %w", bin, err)
		}
		return abs, nil
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("%s: not found on PATH", bin)
	}
	return path, nil
}

// Normalize fills the defaults an absent field implies. binName is the basename
// of the probed binary — the "use the bin" rung of the fallback ladder.
func (m *AppMeta) Normalize(binName string, specPort int) {
	base := strings.TrimSuffix(binName, filepath.Ext(binName))
	if m.Name == "" {
		m.Name = base
	}
	if m.Label == "" {
		m.Label = m.Name
	}
	if m.Mount == "" {
		m.Mount = m.Name
	}
	if m.Mode == "" {
		m.Mode = atlas.WebProxy
	}
	// Metadata reports presentation and transport facts; it does not author
	// access policy. discoverApps applies an explicit operator override later.
	m.Auth = AuthSystem
	// An explicit --app <bin>@<port> wins: the operator is looking at this host
	// and the binary is describing itself in the abstract.
	if specPort != 0 {
		m.Port = specPort
	}
	m.SchemaVersion = MetaSchema
}

// Validate reports why this app cannot become a tile. taken is the set of mounts
// already claimed by builtin and atlas panels.
//
// Unlike atlas.addVerb — which panics, because a verb declaration is the
// author's own compile-time mistake — this is operator input at runtime, so a
// bad app is reported and skipped and the console still comes up.
func (m AppMeta) Validate(taken map[string]bool) error {
	if m.Mount == "" {
		return errors.New("no mount and no name to derive one from")
	}
	if err := validMount(m.Mount); err != nil {
		return err
	}
	if consoleReserved[m.Mount] {
		return fmt.Errorf("mount %q is reserved by the console", m.Mount)
	}
	for _, r := range atlas.ReservedMounts() {
		if m.Mount == r {
			return fmt.Errorf("mount %q is reserved (it could never be published as a cooperative app)", m.Mount)
		}
	}
	if taken[m.Mount] {
		return fmt.Errorf("mount %q is already claimed by another panel", m.Mount)
	}
	// A third party owns its own lifecycle, so it can only ever be proxied —
	// in-process means the console links the handler, which only a Go package
	// compiled into this binary can be.
	if m.Mode != atlas.WebProxy {
		return fmt.Errorf("mode %q: a third-party app must be %q", m.Mode, atlas.WebProxy)
	}
	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("port %d out of range (give one in the meta payload, or as --app <bin>@<port>)", m.Port)
	}
	switch m.Auth {
	case AuthPublic, AuthSystem, AuthCustom:
	default:
		return fmt.Errorf("auth %q: want %s, %s or %s", m.Auth, AuthPublic, AuthSystem, AuthCustom)
	}
	if err := validIcon(m.Icon); err != nil {
		return err
	}
	if err := validDisplay("name", m.Name, metaNameLimit); err != nil {
		return err
	}
	if err := validDisplay("label", m.Label, metaLabelLimit); err != nil {
		return err
	}
	if err := validDisplay("tip", m.Tip, metaTipLimit); err != nil {
		return err
	}
	if err := validStart(m.Start); err != nil {
		return err
	}
	if m.LoginPath != "" {
		if m.Auth != AuthCustom {
			return errors.New("login_path is valid only with operator-selected custom auth")
		}
		if !strings.HasPrefix(m.LoginPath, "/") || strings.HasPrefix(m.LoginPath, "//") || hasControl(m.LoginPath) {
			return fmt.Errorf("login_path %q must be an app-relative absolute path", m.LoginPath)
		}
	}
	return nil
}

func validMount(mount string) error {
	if mount == "." || mount == ".." || len(mount) > metaNameLimit {
		return fmt.Errorf("mount %q must be a safe one-segment slug", mount)
	}
	for i, r := range mount {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return fmt.Errorf("mount %q must be a safe one-segment slug", mount)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validDisplay(field, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d UTF-8 bytes", field, limit)
	}
	if hasControl(value) {
		return fmt.Errorf("%s contains terminal control characters", field)
	}
	return nil
}

func validStart(argv []string) error {
	if len(argv) > 32 {
		return errors.New("start exceeds 32 arguments")
	}
	total := 0
	for i, arg := range argv {
		total += len(arg)
		if (i == 0 && arg == "") || len(arg) > 1024 || hasControl(arg) {
			return errors.New("start contains an empty program, oversized argument, or terminal control")
		}
	}
	if total > metaStartLimit {
		return fmt.Errorf("start exceeds %d UTF-8 bytes", metaStartLimit)
	}
	return nil
}

// validIcon accepts one emoji or SVG path data, and nothing that could escape an
// attribute.
func validIcon(icon string) error {
	if icon == "" {
		return nil
	}
	if utf8.RuneCountInString(icon) <= 2 && !strings.ContainsAny(icon, `<>"'&`) {
		return nil // an emoji, rendered as a text node
	}
	if !iconPath.MatchString(icon) {
		return errors.New("icon must be one emoji or SVG path data (M/L/C/… commands and numbers only)")
	}
	return nil
}

// FromSurface renders an atlas web surface as the same contract a third-party
// app speaks. This is what `bashy <verb> meta` prints, and what keeps the two
// halves from drifting into two schemas.
func FromSurface(name string, w atlas.WebSurface) AppMeta {
	start := []string(nil)
	if len(w.Start) > 0 {
		start = append([]string{"bashy"}, w.Start...)
	}
	m := AppMeta{
		SchemaVersion: MetaSchema,
		Name:          name,
		Label:         w.Label,
		Icon:          w.Icon,
		Tip:           w.Tip,
		Mount:         w.Mount,
		Mode:          w.Mode,
		Port:          w.Port,
		Start:         start,
		Auth:          AuthSystem,
	}
	if m.Label == "" {
		m.Label = name
	}
	if m.Mount == "" {
		m.Mount = name
	}
	return m
}

// WriteMeta emits a meta payload as the JSON a probe expects.
func WriteMeta(w io.Writer, m AppMeta) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
