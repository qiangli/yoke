// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"path/filepath"
	"regexp"
	"strings"
)

// TemplateOpts supplies the space a template is abstracted against.
//
// All three are optional; an empty field simply disables that abstraction
// rather than guessing. A wrong guess here merges two different commands into
// one node, which is silently wrong; declining to abstract only fragments,
// which is visibly weak. Fragmenting is the safe failure.
type TemplateOpts struct {
	RepoRoot string // paths under it stay relative — this is what `about` uses
	HomeDir  string
	TmpDir   string
}

// Template renders argv as a canonical, identity-free string.
//
// This is the node key of the whole execution graph, so the single question it
// answers is: do two invocations MEAN the same thing? `git commit -m "fix a"`
// and `git commit -m "fix b"` must collapse; `go test ./internal/...` and
// `go test ./cmd/...` must not.
//
// It deliberately does NOT reorder flags. Sorting would merge more, but a
// wrong merge conflates two intents and nothing reports it, while a wrong
// split merely leaves two thin nodes that are obviously thin.
func Template(argv []string, opt TemplateOpts) string {
	if len(argv) == 0 {
		return ""
	}

	prog := programName(argv[0], opt)
	parts := []string{prog}

	sub := 0
	if multiVerb[prog] {
		// Subcommands carry the intent: `git commit` and `git push` are not the
		// same command, and a template of "git" for both would be useless.
		//
		// A subcommand must LOOK like one. Taking any two leading non-flag
		// words swallows the first operand — `podman run img:1.2.3` files the
		// image ref as a verb, so every tag becomes its own command. Requiring
		// a bare word keeps `kubectl get pods` while letting `img:1.2.3` and
		// `suites.md` fall through to classification.
		for i := 1; i < len(argv) && sub < 2; i++ {
			if !isSubcommandWord(argv[i]) {
				break
			}
			parts = append(parts, argv[i])
			sub++
		}
	}

	endOpts := false
	for i := 1 + sub; i < len(argv); i++ {
		a := argv[i]

		if endOpts {
			parts = append(parts, classify(a, opt))
			continue
		}
		if a == "--" {
			endOpts = true
			parts = append(parts, a)
			continue
		}

		if !strings.HasPrefix(a, "-") || a == "-" {
			parts = append(parts, classify(a, opt))
			continue
		}

		// `--name=value` — split before anything reads the value.
		if name, val, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			parts = append(parts, name+"="+classifyFor(name, val, prog, opt))
			continue
		}

		// A short cluster with an attached value: `-j8` -> `-j<N>`.
		if flag, val, ok := splitAttached(a); ok {
			parts = append(parts, flag+classifyFor(flag, val, prog, opt))
			continue
		}

		parts = append(parts, a)

		// A flag whose value is the NEXT word.
		if i+1 < len(argv) && takesValue(a, prog) {
			i++
			parts = append(parts, classifyFor(a, argv[i], prog, opt))
		}
	}
	return strings.Join(parts, " ")
}

// programName keeps a repo-local script distinguishable from a system binary.
//
// `./scripts/build.sh` and `/usr/bin/gcc` are not interchangeable, and reducing
// both to a basename would put every repo's `build.sh` on one node.
func programName(arg0 string, opt TemplateOpts) string {
	if opt.RepoRoot != "" && filepath.IsAbs(arg0) {
		if rel, err := filepath.Rel(opt.RepoRoot, arg0); err == nil && !strings.HasPrefix(rel, "..") {
			return "./" + filepath.ToSlash(rel)
		}
	}
	return filepath.Base(arg0)
}

// classifyFor classifies a value with knowledge of the flag that introduced it.
//
// The flag is what makes `-p 2222` a port rather than an anonymous integer, and
// `-m "..."` free text rather than an operand worth keeping.
func classifyFor(flag, val, prog string, opt TemplateOpts) string {
	switch {
	case portFlags[flag]:
		return "<PORT>"
	case textFlags[flag]:
		return "<TEXT>"
	case scriptFlag(flag, prog):
		return "<SCRIPT>"
	case patternFlags[flag]:
		return "<PATTERN>"
	}
	return classify(val, opt)
}

// classify reduces one value to its class.
func classify(v string, opt TemplateOpts) string {
	if v == "" {
		return v
	}
	// user@host and user@host:port, which is the shape the space graph cares
	// about most — it names two entities and a relation in one word.
	if u, rest, ok := strings.Cut(v, "@"); ok && u != "" && rest != "" && !strings.Contains(u, "/") {
		if _, port, hasPort := strings.Cut(rest, ":"); hasPort && isNumeric(port) {
			return "<USER>@<HOST>:<PORT>"
		}
		return "<USER>@<HOST>"
	}
	if isURL(v) {
		return "<URL>"
	}
	if looksPath(v) {
		return classifyPath(v, opt)
	}
	switch {
	case isNumeric(v):
		return "<N>"
	case semverRE.MatchString(v):
		return "<VER>"
	case shaRE.MatchString(v):
		return "<SHA>"
	}
	// A tagged reference — `img:1.2.3`, `repo/img:sha256-…`. The name is the
	// intent and must survive; the tag moves on every release, so leaving it in
	// would file each upgrade under a brand-new node.
	if name, tag, ok := strings.Cut(v, ":"); ok && name != "" && tag != "" {
		switch {
		case semverRE.MatchString(tag):
			return name + ":<VER>"
		case shaRE.MatchString(tag):
			return name + ":<SHA>"
		}
	}
	return v
}

// classifyPath abstracts a path by WHERE it is, not what it is called.
//
// Repo-relative paths survive intact: they are the cross-link into the code
// graph and the only reason `about` edges can exist. Everything outside the
// repo becomes a class, because a home path is identity and a temp path is
// per-run noise that would make every record a singleton.
func classifyPath(v string, opt TemplateOpts) string {
	// A relative path is already free of identity, and it is already the
	// spelling the operator used. Keep it verbatim rather than cleaning it:
	// filepath.Clean turns Go's `./...` package pattern into `...`, which is a
	// different — and meaningless — node.
	if !filepath.IsAbs(v) {
		if hasVolatileSegment(v) {
			return "<TMPPATH>"
		}
		return filepath.ToSlash(v)
	}

	clean := filepath.Clean(v)

	// Repo-relative paths survive intact. They are the cross-link into the code
	// graph and the only reason an `about` edge can exist.
	if opt.RepoRoot != "" && under(clean, opt.RepoRoot) {
		if rel, err := filepath.Rel(opt.RepoRoot, clean); err == nil {
			return "./" + filepath.ToSlash(rel)
		}
	}
	if opt.TmpDir != "" && under(clean, opt.TmpDir) {
		return "<TMPPATH>"
	}
	if hasVolatileSegment(clean) {
		return "<TMPPATH>"
	}
	if opt.HomeDir != "" && under(clean, opt.HomeDir) {
		return "<HOMEPATH>"
	}
	return "<ABSPATH>"
}

// under reports whether path is dir or lies beneath it.
func under(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// hasVolatileSegment spots a per-run path even when it is not under TMPDIR.
//
// A directory named for a random token or a timestamp makes every invocation
// its own template, which is the silent-fragmentation failure this whole file
// is arranged to avoid.
func hasVolatileSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if len(seg) >= 8 && (hexSegRE.MatchString(seg) || digitsRE.MatchString(seg)) {
			return true
		}
		if strings.HasPrefix(seg, "tmp.") || strings.HasPrefix(seg, "T/tmp") {
			return true
		}
	}
	return false
}

func splitAttached(a string) (flag, val string, ok bool) {
	if len(a) < 3 || strings.HasPrefix(a, "--") {
		return "", "", false
	}
	for i := 2; i < len(a); i++ {
		if a[i] >= '0' && a[i] <= '9' {
			return a[:i], a[i:], true
		}
	}
	return "", "", false
}

// isSubcommandWord reports a bare verb — lowercase letters, digits and dashes.
//
// Anything carrying a dot, slash, colon or capital is an operand: a filename,
// an image ref, a URL, a package path. Treating one as a verb is how a
// per-release tag becomes a permanent new node.
func isSubcommandWord(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isURL(s string) bool {
	i := strings.Index(s, "://")
	return i > 0 && i < 12
}

func looksPath(s string) bool {
	return strings.ContainsAny(s, "/\\") || s == "." || s == ".."
}

func scriptFlag(flag, prog string) bool {
	return flag == "-c" && scriptProgs[prog]
}

func takesValue(flag, prog string) bool {
	if portFlags[flag] || textFlags[flag] || patternFlags[flag] || valueFlags[flag] {
		return true
	}
	return scriptFlag(flag, prog)
}

var (
	semverRE = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?([-+][0-9A-Za-z.-]+)?$`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	hexSegRE = regexp.MustCompile(`^[0-9A-Za-z]*[0-9][0-9A-Za-z]*$`)
	digitsRE = regexp.MustCompile(`^\d{8,}$`)
)

// multiVerb lists programs whose first operand is a subcommand rather than an
// argument. Getting this wrong in either direction is cheap and visible: a
// missing entry leaves `git` as one node, a spurious one turns a filename into
// a subcommand.
var multiVerb = map[string]bool{
	"git": true, "go": true, "cargo": true, "npm": true, "pnpm": true, "yarn": true,
	"kubectl": true, "helm": true, "docker": true, "podman": true, "bashy": true,
	"aws": true, "gcloud": true, "az": true, "doctl": true, "gh": true,
	"systemctl": true, "brew": true, "apt": true, "pip": true, "ollama": true,
}

var portFlags = map[string]bool{"-p": true, "-P": true, "--port": true}

var textFlags = map[string]bool{
	"-m": true, "--message": true, "--query": true, "--prompt": true, "--body": true,
}

var patternFlags = map[string]bool{
	"-run": true, "--run": true, "-e": true, "--regexp": true, "--filter": true,
	"--grep": true, "--include": true, "--exclude": true,
}

// valueFlags are flags whose next word is a value rather than an operand.
// Without this the value would be classified anyway, but the FLAG would also
// be mistaken for the last flag of the command.
var valueFlags = map[string]bool{
	"-l": true, "-u": true, "--user": true, "-o": true, "--output": true,
	"-f": true, "--file": true, "-i": true, "-C": true, "--context": true,
	"-n": true, "--namespace": true, "-t": true, "--tag": true,
}

var scriptProgs = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "bashy": true,
	"python": true, "python3": true, "ruby": true, "perl": true, "node": true,
}
