// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import "strings"

// toolPreamble generates a POSIX-sh preflight that a target's `Tools:` clause
// declares — e.g. `Tools: git go:1.25 podman node:20`. Each entry is
// `tool[:min-version]`. The generated check is PREPENDED to the body, so it runs
// wherever the body runs (locally, on a `--mesh` host, or in a `--sandbox`
// container). A missing tool — or one older than its min — prints a clear line
// and exits 3 (precondition failure), so the target fails before its real work.
//
// Version parsing is intentionally generic: the first dotted number in
// `<tool> --version` output (git 2.50.1, go1.26.0, v20.1.0, podman 5.8.0, …),
// compared with `sort -V`. Empty Tools yields an empty preamble.
func toolPreamble(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "# --- dag Tools: preflight (auto-generated) ---\n" +
		"for __dagspec in " + strings.Join(tools, " ") + "; do\n" +
		"  __dagtool=\"${__dagspec%%:*}\"; __dagmin=\"\"\n" +
		"  [ \"$__dagspec\" != \"$__dagtool\" ] && __dagmin=\"${__dagspec#*:}\"\n" +
		"  command -v \"$__dagtool\" >/dev/null 2>&1 || { echo \"dag: required tool not found: $__dagtool\" >&2; exit 3; }\n" +
		"  if [ -n \"$__dagmin\" ]; then\n" +
		"    __dagver=\"$(\"$__dagtool\" --version 2>&1 | grep -oE '[0-9]+(\\.[0-9]+)+' | head -1)\"\n" +
		"    if [ \"$(printf '%s\\n%s\\n' \"$__dagmin\" \"$__dagver\" | sort -V | head -1)\" != \"$__dagmin\" ]; then\n" +
		"      echo \"dag: $__dagtool $__dagver is older than required $__dagmin\" >&2; exit 3\n" +
		"    fi\n" +
		"  fi\n" +
		"done\n" +
		"unset __dagspec __dagtool __dagmin __dagver\n" +
		"# --- end preflight ---\n"
}
