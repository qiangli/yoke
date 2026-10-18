package meet

import (
	"os"
	"strings"
	"unicode"
)

// crossHostPathHint returns a one-line suggestion when a message addressed to
// another host carries an absolute local filesystem path, or "". It never
// rewrites the message and never echoes the path (it may be a hidden one).
func crossHostPathHint(e Event) string {
	if e.Kind != "message" {
		return ""
	}
	to := messageTo(e)
	i := strings.LastIndex(to, "@")
	if i < 0 || i == len(to)-1 {
		return ""
	}
	if sameShortHost(to[i+1:], localShortHost()) {
		return ""
	}
	for _, tok := range strings.Fields(e.Text) {
		if isAbsLocalPath(strings.Trim(tok, "\"'`()[]<>,;")) {
			return "hint: this message names an absolute local path that may not resolve on the receiving host — send a resolvable locator instead"
		}
	}
	return ""
}

func isAbsLocalPath(s string) bool {
	if strings.Contains(s, "://") {
		return false
	}
	if len(s) >= 3 && unicode.IsLetter(rune(s[0])) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	if strings.HasPrefix(s, "//") || !strings.HasPrefix(s, "/") {
		return false
	}
	return len(s) > 1
}

func localShortHost() string {
	h, _ := os.Hostname()
	return h
}

func sameShortHost(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// A short name may be compared with an FQDN. Two distinct FQDNs
	// with the same first label are still different hosts.
	if strings.Contains(a, ".") && strings.Contains(b, ".") {
		return false
	}
	short := func(h string) string { h, _, _ = strings.Cut(h, "."); return h }
	return short(a) == short(b)
}
