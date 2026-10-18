package meet

import (
	"strings"
	"testing"
)

func hintFor(t *testing.T, to, text string) (string, string) {
	t.Helper()
	e := Event{Round: 1, Speaker: "a", Kind: "message", Text: text}
	e.To = to
	var b strings.Builder
	writeContributions(&b, &State{ID: "m1"}, []Event{e}, "")
	return crossHostPathHint(e), b.String()
}

func TestLocatorCrossHostPathHint(t *testing.T) {
	h, out := hintFor(t, "bob@far-away-host", "look at /Users/x/.secret/notes.md now")
	if !strings.Contains(h, "receiving host") || strings.Contains(h, ".secret") {
		t.Fatalf("bad hint: %q", h)
	}
	if !strings.Contains(out, h) || !strings.Contains(out, "/.secret/notes.md") {
		t.Fatalf("message must be preserved and hint shown: %s", out)
	}
}

func TestLocatorNoHint(t *testing.T) {
	local := localShortHost()
	for name, c := range map[string][2]string{
		"locator":   {"bob@far-away-host", "see dir:/tmp/x/file.go"},
		"same host": {"bob@" + local, "see /tmp/x/file.go"},
		"no host":   {"bob", "see /tmp/x/file.go"},
		"url":       {"bob@far-away-host", "see https://example.com/a/b"},
		"prose":     {"bob@far-away-host", "and/or either"},
	} {
		if h, _ := hintFor(t, c[0], c[1]); h != "" {
			t.Errorf("%s: unexpected hint %q", name, h)
		}
	}
}

func TestSameShortHostKeepsDistinctDomains(t *testing.T) {
	if sameShortHost("node.one.example", "node.two.example") {
		t.Fatal("different FQDNs with the same first label treated as one host")
	}
	if !sameShortHost("node", "node.one.example") {
		t.Fatal("short name and matching FQDN treated as different hosts")
	}
}
