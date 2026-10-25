package fleet

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/spacetime"
)

// CommandMatch is one generic input-support declaration. Path is an RFC 6901
// JSON pointer and Pattern is an RE2 expression matched against the pointed-to
// scalar. The vocabulary is deliberately open: websites, media types and
// future provider domains remain record data rather than core enums.
type CommandMatch struct {
	Path    string `yaml:"path" json:"path" doc:"RFC 6901 JSON pointer into the provider request"`
	Pattern string `yaml:"pattern" json:"pattern" doc:"RE2 expression the pointed-to scalar must match"`
}

var providerRoleRE = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)

func (c Command) validateApplicability() error {
	seen := map[string]bool{}
	for _, role := range c.Provides {
		if !providerRoleRE.MatchString(role) {
			return fmt.Errorf("provides value %q is not an open dotted identifier", role)
		}
		if seen[role] {
			return fmt.Errorf("provides value %q is duplicated", role)
		}
		seen[role] = true
	}
	if len(c.Accepts) > 0 && len(c.Provides) == 0 {
		return fmt.Errorf("accepts requires at least one provides role")
	}
	for i, m := range c.Accepts {
		if m.Path != "" && !strings.HasPrefix(m.Path, "/") {
			return fmt.Errorf("accepts.%d.path %q is not an RFC 6901 JSON pointer", i, m.Path)
		}
		if _, err := decodeJSONPointer(m.Path); err != nil {
			return fmt.Errorf("accepts.%d.path: %w", i, err)
		}
		if m.Pattern == "" {
			return fmt.Errorf("accepts.%d.pattern is empty", i)
		}
		if _, err := regexp.Compile(m.Pattern); err != nil {
			return fmt.Errorf("accepts.%d.pattern: %w", i, err)
		}
	}
	if c.Requires != "" {
		if _, err := spacetime.ParseRequires(c.Requires); err != nil {
			return fmt.Errorf("requires: %w", err)
		}
	}
	return nil
}

// ProvidesRole reports whether the command declares an open provider role.
func (c Command) ProvidesRole(role string) bool {
	for _, provided := range c.Provides {
		if provided == role {
			return true
		}
	}
	return false
}

// Applicability evaluates the command's existing OS declaration and its
// skill-compatible requires expression at one host coordinate. It returns an
// actionable first failure, matching the skills catalog's verdict shape.
func (c Command) Applicability(ps *spacetime.ProbeSet) spacetime.Verdict {
	if ps == nil {
		ps = spacetime.DefaultProbes(spacetime.NopCache())
	}
	if len(c.OS) > 0 {
		os, _ := ps.Value("os")
		found := false
		for _, supported := range c.OS {
			if supported == os {
				found = true
				break
			}
		}
		if !found {
			return spacetime.Verdict{Applicable: false, Failing: fmt.Sprintf("os=%s: os=%s", strings.Join(c.OS, ","), absentValue(os))}
		}
	}
	if c.Requires == "" {
		return spacetime.Verdict{Applicable: true}
	}
	req, err := spacetime.ParseRequires(c.Requires)
	if err != nil {
		return spacetime.Verdict{Applicable: false, Failing: "requires invalid: " + err.Error()}
	}
	return req.Eval(ps)
}

// AcceptsJSON reports whether every declared matcher accepts request. The
// reason names the declaration that failed so selectors can explain why an
// otherwise relevant provider was skipped.
func (c Command) AcceptsJSON(request any) (bool, string) {
	for _, match := range c.Accepts {
		value, ok := jsonPointer(request, match.Path)
		if !ok {
			return false, fmt.Sprintf("request has no scalar at %s", match.Path)
		}
		re, err := regexp.Compile(match.Pattern)
		if err != nil { // Save/parse validation normally makes this unreachable.
			return false, fmt.Sprintf("invalid pattern for %s: %v", match.Path, err)
		}
		if !re.MatchString(value) {
			return false, fmt.Sprintf("%s=%q does not match %q", match.Path, value, match.Pattern)
		}
	}
	return true, ""
}

func decodeJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("must start with /")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, part := range parts {
		var b strings.Builder
		for j := 0; j < len(part); j++ {
			if part[j] != '~' {
				b.WriteByte(part[j])
				continue
			}
			if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
				return nil, fmt.Errorf("contains an invalid ~ escape")
			}
			j++
			if part[j] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		parts[i] = b.String()
	}
	return parts, nil
}

func jsonPointer(root any, pointer string) (string, bool) {
	parts, err := decodeJSONPointer(pointer)
	if err != nil {
		return "", false
	}
	value := root
	for _, part := range parts {
		switch node := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = node[part]
			if !ok {
				return "", false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return "", false
			}
			value = node[i]
		default:
			return "", false
		}
	}
	switch scalar := value.(type) {
	case string:
		return scalar, true
	case json.Number:
		return scalar.String(), true
	case float64, bool:
		return fmt.Sprint(scalar), true
	default:
		return "", false
	}
}

func absentValue(value string) string {
	if value == "" {
		return "absent"
	}
	return value
}
