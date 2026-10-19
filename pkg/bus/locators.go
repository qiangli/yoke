package bus

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/lens"
	"github.com/qiangli/yoke/pkg/ref"
)

// The board renders on the reader's host. On Windows its suggested command
// must use the same S4 PowerShell row as Meet, not POSIX shell quoting.
var localCommandOS = runtime.GOOS

// MessageRef is a canonical locator found in a message body (Sprint 446 S16,
// communication by address). It is a READ-SIDE annotation: the body is stored
// and shown unchanged, nothing is fetched or executed, and the recipient
// follows Ref explicitly with the same tool (`lens`, `define`, ...).
type MessageRef struct {
	// Ref is the locator to follow. For a host-local locator written on
	// another host it carries that host as an explicit @scope, so following
	// it can never silently read the recipient's own filesystem.
	Ref string `json:"ref"`
	// Text is the locator exactly as the sender wrote it.
	Text   string `json:"text"`
	Scheme string `json:"scheme"`
	// Host is the host the locator resolves on, when it is host-local and the
	// message came from another host (or names one with @host).
	Host string `json:"host,omitempty"`
	// CrossHost reports that Host is not this host.
	CrossHost bool `json:"cross_host,omitempty"`
	// Note is a short caveat for the reader; empty when there is none.
	Note string `json:"note,omitempty"`
}

// hostLocalSchemes name resources on the filesystem or kernel of ONE host: an
// unscoped one means "on the host that wrote it".
var hostLocalSchemes = map[string]bool{
	"dir": true, "tar": true, "zip": true, "gzip": true, "git": true,
	"ast": true, "graph": true, "json": true, "yaml": true, "md": true,
	"pdf": true, "proc": true, "net": true,
}

// localHostname is this host's name; a variable so tests can pin it.
var localHostname = func() string { h, _ := os.Hostname(); return h }

// MessageLocators returns the canonical locators named in body, in order and
// without duplicates. from is the post's sender; a `<name>@<host>` sender
// marks the host its unscoped host-local locators belong to.
//
// Only QUALIFIED tokens count (`scheme:target`, with a scheme the locator
// grammar declares): a bare word or path also parses as `dir:` and would turn
// every sentence into references. URLs and anything ParseLocator rejects are
// left as prose.
func MessageLocators(body, from string) []MessageRef {
	origin := senderHost(from)
	local := localHostname()
	var out []MessageRef
	seen := map[string]bool{}
	for _, tok := range strings.Fields(body) {
		text := trimLocatorToken(tok)
		if !qualifiedLocator(text) {
			continue
		}
		l, err := ref.ParseLocator(text)
		if err != nil || l.Target == "" {
			continue
		}
		m := MessageRef{Text: text, Scheme: l.Scheme}
		switch {
		case l.ScopeKind == "host":
			m.Host = l.ScopeValue
		case l.ScopeKind == "" && hostLocalSchemes[l.Scheme] && origin != "":
			m.Host = origin
		}
		if m.Host != "" && !sameHost(m.Host, local) {
			m.CrossHost = true
			if l.ScopeKind == "" {
				// Pin the writer's host so the reference cannot be followed
				// against this host's filesystem by accident.
				l.ScopeKind, l.ScopeValue = "host", m.Host
				m.Note = "written on " + m.Host + "; resolves there, not on this host"
			} else {
				m.Note = "resolves on " + m.Host + ", not on this host"
			}
		}
		m.Ref = l.String()
		if seen[m.Ref] {
			continue
		}
		seen[m.Ref] = true
		out = append(out, m)
	}
	return out
}

// trimLocatorToken strips the prose punctuation a locator is quoted or ended
// with. `!`, `@` and `:` are grammar, so a trailing one stays and the parser
// decides.
func trimLocatorToken(tok string) string {
	tok = strings.TrimLeft(tok, "\"'`([<{")
	return strings.TrimRight(tok, "\"'`)]>},.;?")
}

// qualifiedLocator reports a token spelled `scheme:...` with a lowercase
// multi-letter scheme (a single letter is a Windows drive), and not a URL.
func qualifiedLocator(s string) bool {
	if strings.Contains(s, "://") {
		return false
	}
	s = strings.TrimPrefix(s, ref.URNPrefix)
	scheme, _, ok := strings.Cut(s, ":")
	if !ok || len(scheme) < 2 {
		return false
	}
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		if !(c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '-')) {
			return false
		}
	}
	return true
}

// senderHost is the host of a `<name>@<host>` sender, or "".
func senderHost(from string) string {
	if i := strings.LastIndex(from, "@"); i >= 0 && i < len(from)-1 {
		return from[i+1:]
	}
	return ""
}

// sameHost compares host names ignoring case and any domain suffix.
func sameHost(a, b string) bool {
	short := func(h string) string { h, _, _ = strings.Cut(strings.ToLower(h), "."); return h }
	return a != "" && b != "" && short(a) == short(b)
}

// postWithRefs is a Post as read back with its locators: Post's own fields,
// unchanged, plus "refs" when the body names any.
type postWithRefs struct {
	Post
	Refs []MessageRef `json:"refs,omitempty"`
}

func withRefs(p Post) postWithRefs {
	return postWithRefs{Post: p, Refs: MessageLocators(p.Body, p.From)}
}

// refsLine renders a post's locators for the human view, or "".
func refsLine(refs []MessageRef) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Ref
		if r.Note != "" {
			parts[i] += " (" + r.Note + ")"
		}
	}
	reads := make([]string, len(refs))
	for i, r := range refs {
		if command := readCommand(r.Ref); command != "" {
			reads[i] = "`" + command + "`"
		}
	}
	return "refs: " + strings.Join(parts, ", ") + " — read: " + strings.Join(reads, ", ")
}

func readCommand(ref string) string {
	if localCommandOS != "windows" {
		return "lens " + shellQuoteRef(ref)
	}
	data, err := json.Marshal(map[string]any{"name": "lens", "kind": "command", "exec": []string{"lens", ref}})
	if err != nil {
		return ""
	}
	res, err := lens.Convert(context.Background(), lens.Request{From: "json", To: "pwsh", Data: data})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(res.Data))
}

// shellQuoteRef single-quotes a locator only when it holds shell-special bytes.
func shellQuoteRef(s string) string {
	if !strings.ContainsAny(s, " \t!$&;|<>()*?[]{}'\"`\\#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
