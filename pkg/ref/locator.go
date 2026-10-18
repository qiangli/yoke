package ref

import (
	"fmt"
	"net/url"
	"strings"
)

// Locator names a resource independently of permission to read it. Scope is a
// single trailing location, not a second resource or an access credential.
type Locator struct {
	Scheme     string
	Target     string
	Revision   string
	Inner      []string
	ScopeKind  string
	ScopeValue string
	URN        bool
}

var locatorSchemes = map[string]bool{
	"dir": true, "tar": true, "zip": true, "gzip": true, "git": true,
	"ast": true, "graph": true, "json": true, "yaml": true, "md": true,
	"pdf": true, "proc": true, "net": true, "peer": true, "sphere": true,
	"story": true, "claim": true, "oci": true, "k8s": true, "dag": true,
}

// ParseLocator parses the declared v1 schemes and existing entity refs.
// Additional recipe schemes can be passed to ParseLocatorWithSchemes.
func ParseLocator(s string) (Locator, error) { return ParseLocatorWithSchemes(s, nil) }

func ParseLocatorWithSchemes(s string, extra map[string]bool) (Locator, error) {
	if s == "" {
		return Locator{}, fmt.Errorf("locator: empty resource")
	}
	resource, scope, hasScope := strings.Cut(s, "@")
	l := Locator{}
	if hasScope {
		if strings.Contains(scope, "@") || scope == "" {
			return l, fmt.Errorf("locator: multiple or empty location scopes")
		}
		kind, value, keyed := strings.Cut(scope, ":")
		if keyed {
			switch kind {
			case "ws", "sandbox", "k8s", "cloud":
			default:
				return l, fmt.Errorf("locator: unknown location scope %q", kind)
			}
			l.ScopeKind = kind
		} else {
			l.ScopeKind = "host"
			value = scope
		}
		var err error
		l.ScopeValue, err = decodeLocatorPart(value)
		if err != nil {
			return Locator{}, err
		}
		if l.ScopeValue == "" {
			return Locator{}, fmt.Errorf("locator: empty location scope")
		}
	}
	// Windows drive paths are local paths, even though they contain a colon.
	if len(resource) >= 3 && isAlpha(resource[0]) && resource[1] == ':' && (resource[2] == '\\' || resource[2] == '/') {
		if len(resource) != len(s) || strings.Contains(resource, "!") {
			return Locator{}, fmt.Errorf("locator: ambiguous Windows path")
		}
		l.Scheme = "dir"
		l.Target = resource
		return l, nil
	}
	parts := strings.Split(resource, "!/")
	if strings.Contains(parts[0], "!") {
		return Locator{}, fmt.Errorf("locator: raw ! must be escaped")
	}
	for _, p := range parts[1:] {
		if p == "" || strings.Contains(p, "!") {
			return Locator{}, fmt.Errorf("locator: empty or unescaped inner path")
		}
	}
	head := parts[0]
	if strings.HasPrefix(head, URNPrefix) {
		l.URN = true
		head = strings.TrimPrefix(head, URNPrefix)
	}
	scheme, target, qualified := strings.Cut(head, ":")
	if !qualified {
		if len(parts) > 1 {
			return Locator{}, fmt.Errorf("locator: container descent needs a scheme")
		}
		l.Scheme = "dir"
		var err error
		l.Target, err = decodeLocatorPart(head)
		return l, err
	}
	if !locatorSchemes[scheme] && !Known(Kind(scheme)) && !extra[scheme] {
		return Locator{}, fmt.Errorf("locator: unsupported scheme %q", scheme)
	}
	if l.URN && !Known(Kind(scheme)) {
		return Locator{}, fmt.Errorf("locator: URN requires a known ref kind")
	}
	l.Scheme = scheme
	if scheme == "git" {
		var path string
		l.Revision, path, qualified = strings.Cut(target, ":")
		if !qualified || l.Revision == "" {
			return Locator{}, fmt.Errorf("locator: git requires revision:path")
		}
		var err error
		l.Revision, err = decodeLocatorPart(l.Revision)
		if err != nil {
			return Locator{}, err
		}
		target = path
		if strings.Contains(target, ":") {
			return Locator{}, fmt.Errorf("locator: raw colon in target must be escaped")
		}
	} else if strings.Contains(target, ":") {
		return Locator{}, fmt.Errorf("locator: raw colon in target must be escaped")
	}
	var err error
	l.Target, err = decodeLocatorPart(target)
	if err != nil {
		return Locator{}, err
	}
	if Known(Kind(scheme)) && l.Target == "" {
		return Locator{}, ErrEmptyID
	}
	for _, p := range parts[1:] {
		if strings.Contains(p, ":") {
			return Locator{}, fmt.Errorf("locator: raw colon in inner path must be escaped")
		}
		v, err := decodeLocatorPart(p)
		if err != nil {
			return Locator{}, err
		}
		l.Inner = append(l.Inner, v)
	}
	return l, nil
}

func isAlpha(b byte) bool { return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }

func decodeLocatorPart(s string) (string, error) {
	if strings.ContainsAny(s, "!@:\\") {
		return "", fmt.Errorf("locator: reserved character must be escaped")
	}
	v, err := url.PathUnescape(s)
	if err != nil {
		return "", fmt.Errorf("locator: invalid percent escape: %w", err)
	}
	return v, nil
}

func encodeLocatorPart(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '!', '@', ':', '\\':
			fmt.Fprintf(&b, "%%%02X", s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (l Locator) String() string {
	var s string
	if l.Scheme == "dir" && l.ScopeKind == "" && len(l.Inner) == 0 && !strings.ContainsAny(l.Target, ":!@%\\") {
		s = l.Target
	} else if l.Scheme == "git" {
		s = "git:" + encodeLocatorPart(l.Revision) + ":" + encodeLocatorPart(l.Target)
	} else {
		s = l.Scheme + ":" + encodeLocatorPart(l.Target)
	}
	if l.URN {
		s = URNPrefix + s
	}
	for _, p := range l.Inner {
		s += "!/" + encodeLocatorPart(p)
	}
	if l.ScopeKind == "host" {
		s += "@" + encodeLocatorPart(l.ScopeValue)
	} else if l.ScopeKind != "" {
		s += "@" + l.ScopeKind + ":" + encodeLocatorPart(l.ScopeValue)
	}
	return s
}
