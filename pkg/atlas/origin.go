// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import "fmt"

// gnuCoreutilsUpstream is the GNU coreutils 9.x inventory. Membership is what
// makes a tool OriginGNU; the three names bashy does not implement (chroot,
// coreutils, runcon) are kept so `commands --gnu` can report them missing.
var gnuCoreutilsUpstream = []string{
	"arch", "b2sum", "base32", "base64", "basename", "basenc", "cat", "chcon",
	"chgrp", "chmod", "chown", "chroot", "cksum", "comm", "coreutils", "cp",
	"csplit", "cut", "date", "dd", "df", "dir", "dircolors", "dirname", "du",
	"echo", "env", "expand", "expr", "factor", "false", "fmt", "fold", "groups",
	"head", "hostid", "hostname", "id", "install", "join", "kill", "link", "ln",
	"logname", "ls", "md5sum", "mkdir", "mkfifo", "mknod", "mktemp", "mv", "nice",
	"nl", "nohup", "nproc", "numfmt", "od", "paste", "pathchk", "pinky", "pr",
	"printenv", "printf", "ptx", "pwd", "readlink", "realpath", "rm", "rmdir",
	"runcon", "seq", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum",
	"shred", "shuf", "sleep", "sort", "split", "stat", "stdbuf", "stty", "sum",
	"sync", "tac", "tail", "tee", "test", "timeout", "touch", "tr", "true",
	"truncate", "tsort", "tty", "uname", "unexpand", "uniq", "unlink", "uptime",
	"users", "vdir", "wc", "who", "whoami", "yes",
}

// posixRequired is the 116-name POSIX-required utility set, the projection
// `posix-gate spec` certifies. Source of truth is
// docs/posix-required-commands.tsv; origin_test.go fails when the two drift.
var posixRequired = []string{
	"alias", "ar", "at", "awk", "basename", "batch", "bc", "bg", "cat", "cd",
	"chgrp", "chmod", "chown", "cksum", "cmp", "comm", "command", "cp",
	"crontab", "csplit", "ctags", "cut", "date", "dd", "df", "diff", "dirname",
	"du", "echo", "ed", "env", "ex", "expand", "expr", "false", "fc", "fg",
	"file", "find", "fold", "getconf", "getopts", "grep", "hash", "head",
	"iconv", "id", "jobs", "join", "kill", "ln", "locale", "localedef",
	"logger", "logname", "lp", "ls", "m4", "mailx", "make", "man", "mesg",
	"mkdir", "mkfifo", "more", "mv", "newgrp", "nice", "nm", "nohup", "od",
	"paste", "patch", "pathchk", "pax", "pr", "printf", "ps", "pwd", "read",
	"renice", "rm", "rmdir", "sed", "sh", "sleep", "sort", "split", "strings",
	"strip", "stty", "tabs", "tail", "talk", "tee", "test", "time", "touch",
	"tput", "tr", "true", "tsort", "tty", "umask", "unalias", "uname",
	"unexpand", "uniq", "uudecode", "uuencode", "vi", "wait", "wc", "who",
	"write", "xargs",
}

// bashyTools are the in-process tools that are bashy's own inventions —
// neither GNU, nor classic Unix, nor a wrapped external. Everything else in
// the tool table that is not GNU and not a managed external is OriginUnix.
// (Verbs need no list: a verb is OriginBashy unless its Subclass says it is
// exec'd — see classifyOrigins.)
var bashyTools = []string{
	"ast", "graph", "resources", "posix-gate",
	"browser", "fetch",
	"tokens", "duration", "tz", "ntp", "sntp", "clip",
}

var (
	gnuCoreutilsSet  = sliceSet(gnuCoreutilsUpstream)
	posixRequiredSet = sliceSet(posixRequired)
)

func sliceSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// classifyOrigins stamps Origin and Posix on every table entry. It runs after
// the tables and the subclass/cap passes and before the alias pass, so an
// alias copies its target's origin. Unknown names in bashyTools panic: a
// stale list must not silently reclassify a tool as classic Unix.
func classifyOrigins() {
	bashy := sliceSet(bashyTools)
	for n := range bashy {
		if _, ok := tools[n]; !ok {
			panic(fmt.Sprintf("atlas: bashyTools names unknown tool %q", n))
		}
	}
	for n, e := range tools {
		switch {
		case e.Subclass == SubclassManagedExternal || e.Subclass == SubclassProvisioner:
			e.Origin = OriginExternal
		case bashy[n]:
			e.Origin = OriginBashy
		case gnuCoreutilsSet[n]:
			e.Origin = OriginGNU
		default:
			e.Origin = OriginUnix
		}
		e.Posix = posixRequiredSet[n]
		tools[n] = e
	}
	// A tool alias (`[` → test) is a second spelling, not a second provenance.
	for n, e := range tools {
		if t, ok := tools[e.AliasOf]; ok && e.AliasOf != "" {
			e.Origin = t.Origin
			tools[n] = e
		}
	}
	for n, e := range verbs {
		switch {
		case e.Subclass == SubclassManagedExternal || e.Subclass == SubclassProvisioner:
			e.Origin = OriginExternal
		default:
			e.Origin = OriginBashy
		}
		e.Posix = posixRequiredSet[n]
		verbs[n] = e
	}
}
