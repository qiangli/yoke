package fleet

import (
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
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
