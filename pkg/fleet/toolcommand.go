package fleet

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
)

// CanonicalToolCommands is the cross-tool command vocabulary, listed once
// (docs/tool-commands-design.md mirrors it). A tool that exposes one of
// these features registers it under this name whatever its own spelling,
// so `claude:review` and `codex:review` are one service on two tools.
//
// It is a vocabulary, not a whitelist: any name is allowed, and a name
// outside it only warns (ValidateCommands).
var CanonicalToolCommands = []string{
	"compact",
	"deep-research",
	"exit",
	"init",
	"plan",
	"resume",
	"review",
	"security-review",
	"status",
}

// IsCanonicalToolCommand reports whether name is in the vocabulary.
func IsCanonicalToolCommand(name string) bool {
	return slices.Contains(CanonicalToolCommands, name)
}

// Command returns the tool's declared command by canonical name.
func (t Tool) Command(name string) (ToolCommand, bool) {
	for _, c := range t.Commands {
		if c.Name == name {
			return c, true
		}
	}
	return ToolCommand{}, false
}

// ValidateCommands checks the commands block. errs are declarations a
// runner must refuse; warns are true but not disqualifying (a name outside
// the canonical vocabulary). Neither drops the tool.
func (t Tool) ValidateCommands() (errs []error, warns []string) {
	seen := map[string]bool{}
	for i, c := range t.Commands {
		label := c.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("tool %s: command %s: %s", t.Name, label, fmt.Sprintf(format, a...)))
		}
		seenEffects := map[string]bool{}
		for _, effect := range c.Effects {
			if strings.TrimSpace(effect) != effect || effect == "" || seenEffects[effect] {
				bad("invalid or duplicate effect %q", effect)
			}
			seenEffects[effect] = true
		}
		if c.Name == "" {
			bad("name is empty")
		} else if seen[c.Name] {
			bad("duplicate name")
		} else {
			seen[c.Name] = true
			if !IsCanonicalToolCommand(c.Name) {
				warns = append(warns, fmt.Sprintf("tool %s: command %s is not a canonical name (%s); allowed, but other tools will not find it under the same name",
					t.Name, c.Name, strings.Join(CanonicalToolCommands, ", ")))
			}
		}
		if c.Mode != ToolCommandHTTP && strings.TrimSpace(c.Slash) == "" {
			bad("slash is empty")
		}
		switch c.Mode {
		case ToolCommandPrint:
			if len(c.Steps) > 0 || c.Quit != "" {
				bad("steps and quit are tui only")
			}
		case ToolCommandTUI:
			if strings.TrimSpace(t.CLI.Launch.SteerExec) == "" {
				bad("mode tui needs the tool's steer_exec")
			}
			if c.Exec != "" {
				bad("exec is print only")
			}
		case ToolCommandHTTP:
			if c.HTTP == nil {
				bad("http mode needs http")
				break
			}
			if c.Exec != "" || len(c.Steps) > 0 || c.Quit != "" {
				bad("exec, steps and quit are not http")
			}
			if err := c.HTTP.Validate(c.Effects); err != nil {
				bad("%v", err)
			}
		default:
			bad("mode %q (want print, tui or http)", c.Mode)
		}
		if c.Timeout != "" {
			if _, err := time.ParseDuration(c.Timeout); err != nil {
				bad("timeout %q: %v", c.Timeout, err)
			}
		}
		if err := validateToolCommandOutput(c.Output); err != nil {
			bad("%v", err)
		}
		for j, s := range c.Steps {
			n := 0
			for _, v := range []string{s.Say, s.Key, s.WaitIdle} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				bad("step %d: set exactly one of say, key, wait_idle", j)
				continue
			}
			if s.WaitIdle != "" {
				if _, err := time.ParseDuration(s.WaitIdle); err != nil {
					bad("step %d: wait_idle %q: %v", j, s.WaitIdle, err)
				}
			}
		}
	}
	return errs, warns
}

// Validate checks the transport declaration without opening a connection.
func (h ToolCommandHTTPAction) Validate(effects []string) error {
	method := strings.ToUpper(strings.TrimSpace(h.Method))
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return fmt.Errorf("http.method %q (want GET, POST, PUT, PATCH or DELETE)", h.Method)
	}
	u, err := url.Parse(strings.TrimSpace(h.URL))
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("http.url %q must be an absolute http URL without userinfo", h.URL)
	}
	if h.Path != "" && (!strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, "?#")) {
		return fmt.Errorf("http.path %q must begin with / and have no query or fragment", h.Path)
	}
	if (strings.TrimSpace(h.TokenFile) == "") == (strings.TrimSpace(h.TokenRef) == "") {
		return fmt.Errorf("http needs exactly one of token_file or token_ref")
	}
	if !loopbackHost(u.Hostname()) && !(slices.Contains(effects, atlas.EffNet) && slices.Contains(effects, atlas.EffRemote)) {
		return fmt.Errorf("http.url %q is not loopback; remote HTTP requires declared effects net and remote for approval policy", h.URL)
	}
	return h.validateArgs()
}

// MutatingEffects are the effect atoms that change state the caller cannot
// take back by not looking: a command declaring any of them is admitted only
// with explicit operator confirmation (NeedsConfirmation).
var MutatingEffects = []string{atlas.EffWrite, atlas.EffDestroy, atlas.EffPersist, atlas.EffPriv, atlas.EffSpend}

// NeedsConfirmation is the ONE admission rule for a tool command, shared by
// toolcmd's runner and every front door that offers --yes: a command that
// declares confirm: true or a mutating effect runs only when the operator
// confirmed it. reason names why, for the refusal message.
func (c ToolCommand) NeedsConfirmation() (need bool, reason string) {
	var hit []string
	for _, e := range c.Effects {
		if slices.Contains(MutatingEffects, e) {
			hit = append(hit, e)
		}
	}
	switch {
	case len(hit) > 0:
		return true, "declares mutating effect " + strings.Join(hit, ",")
	case c.Confirm:
		return true, "declares confirm: true"
	}
	return false, ""
}

var (
	httpArgName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	httpArgField = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func (h ToolCommandHTTPAction) validateArgs() error {
	if len(h.Args) == 0 {
		return nil
	}
	if bodyHasArgsToken(h.Body) {
		return fmt.Errorf("http.body uses {args} but http.args is declared; bind each value through args")
	}
	names := map[string]bool{}
	var fields []string
	for i, a := range h.Args {
		if !httpArgName.MatchString(a.Name) {
			return fmt.Errorf("http.args[%d]: name %q must match %s", i, a.Name, httpArgName)
		}
		if names[a.Name] {
			return fmt.Errorf("http.args[%d]: duplicate name %q", i, a.Name)
		}
		names[a.Name] = true
		field := a.BodyField()
		for _, seg := range strings.Split(field, ".") {
			if !httpArgField.MatchString(seg) {
				return fmt.Errorf("http.args %s: field %q must be dotted segments of [A-Za-z0-9_-]", a.Name, field)
			}
		}
		for _, f := range fields {
			if f == field || strings.HasPrefix(f, field+".") || strings.HasPrefix(field, f+".") {
				return fmt.Errorf("http.args %s: field %q overlaps field %q", a.Name, field, f)
			}
		}
		fields = append(fields, field)
		if err := bodyFieldFree(h.Body, field); err != nil {
			return fmt.Errorf("http.args %s: %v", a.Name, err)
		}
		switch a.Type {
		case "", ToolCommandArgString, ToolCommandArgInteger, ToolCommandArgBoolean:
		default:
			return fmt.Errorf("http.args %s: type %q (want string, integer or boolean)", a.Name, a.Type)
		}
		if len(a.Enum) > 0 && a.Type != "" {
			return fmt.Errorf("http.args %s: set type or enum, not both (enum values carry their own JSON type)", a.Name)
		}
		for k, v := range a.Enum {
			if k == "" {
				return fmt.Errorf("http.args %s: enum has an empty key", a.Name)
			}
			switch v.(type) {
			case string, bool, int, int64, uint64, float64:
			default:
				return fmt.Errorf("http.args %s: enum %q maps to %T (want a string, number or boolean)", a.Name, k, v)
			}
		}
		if a.MaxLen < 0 {
			return fmt.Errorf("http.args %s: max_len %d is negative", a.Name, a.MaxLen)
		}
		if a.Default != "" {
			if a.Required {
				return fmt.Errorf("http.args %s: required and default are exclusive", a.Name)
			}
			if _, err := a.Project(a.Default); err != nil {
				return fmt.Errorf("http.args %s: default: %v", a.Name, err)
			}
		}
	}
	return nil
}

// Project converts one caller value into the JSON value the argument sends:
// the enum's mapped value, or the value parsed as the declared type. It is
// the whole of the argument's input language — nothing else is accepted.
func (a ToolCommandHTTPArg) Project(v string) (any, error) {
	limit := a.MaxLen
	if limit == 0 {
		limit = ToolCommandArgMaxLen
	}
	if len(v) > limit {
		return nil, fmt.Errorf("value is %d bytes (max %d)", len(v), limit)
	}
	if len(a.Enum) > 0 {
		out, ok := a.Enum[v]
		if !ok {
			allowed := make([]string, 0, len(a.Enum))
			for k := range a.Enum {
				allowed = append(allowed, k)
			}
			slices.Sort(allowed)
			return nil, fmt.Errorf("value %q is not one of %s", v, strings.Join(allowed, "|"))
		}
		return out, nil
	}
	switch a.Type {
	case ToolCommandArgInteger:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q is not an integer", v)
		}
		return n, nil
	case ToolCommandArgBoolean:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a boolean", v)
		}
		return b, nil
	}
	return v, nil
}

func bodyHasArgsToken(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "{args}")
	case map[string]any:
		for _, v := range x {
			if bodyHasArgsToken(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if bodyHasArgsToken(v) {
				return true
			}
		}
	}
	return false
}

// bodyFieldFree reports an error when setting the dotted field would
// overwrite a literal body value or descend through a non-object.
func bodyFieldFree(body map[string]any, field string) error {
	cur := body
	segs := strings.Split(field, ".")
	for i, seg := range segs {
		v, ok := cur[seg]
		if !ok {
			return nil
		}
		if i == len(segs)-1 {
			return fmt.Errorf("field %q would overwrite a literal body value", field)
		}
		next, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("field %q descends through a non-object body value", field)
		}
		cur = next
	}
	return nil
}

func loopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func validateToolCommandOutput(out string) error {
	switch {
	case out == "" || out == "turn" || out == "transcript":
		return nil
	case strings.HasPrefix(out, "file:"):
		glob := strings.TrimPrefix(out, "file:")
		if glob == "" {
			return fmt.Errorf("output %q: empty glob", out)
		}
		if _, err := filepath.Match(glob, ""); err != nil {
			return fmt.Errorf("output %q: %v", out, err)
		}
		return nil
	default:
		return fmt.Errorf("output %q (want turn, transcript or file:<glob>)", out)
	}
}
