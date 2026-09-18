package secrets

import (
	"bufio"
	"bytes"
	"strings"
)

// ParseEnv parses the `export NAME='value'` lines produced by `bashy secret env`
// back into a NAME->value map. It is the inverse of the renderer: comments (#) and
// blank lines are skipped, and single-quoted values are unescaped with the same
// convention shellSingleQuote emits ('\” -> '), so any value round-trips exactly.
//
// This lets a supervisor (e.g. outpost) REUSE `secrets env` — with its cloudbox
// fetch, local binding-template resolution, and offline-cache fallback — and
// inject the result into a child process's environment, keeping that child fully
// DECOUPLED from the vault: it just reads plain env vars (GITHUB_TOKEN, …) and
// never learns the vault key names or the host/provider naming convention.
func ParseEnv(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:eq])
		if !validName(name) {
			continue
		}
		out[name] = unquoteSingle(strings.TrimSpace(line[eq+1:]))
	}
	return out
}

// unquoteSingle reverses shellSingleQuote: strip the wrapping single quotes and
// collapse the '\” escape back to a literal quote. A bare (unquoted) value is
// returned unchanged.
func unquoteSingle(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}
