// Package all registers the WHOLE bashy userland via blank imports: the
// certified required set (github.com/qiangli/coreutils/cmds/all — the 116
// POSIX-required names ∪ GNU coreutils) plus every yoke applet below — the
// agentic and non-POSIX tools bashy adds (ast, browser, fetch, jq, tar, tree,
// which, …). A consumer that wants everything imports this one package; a
// consumer that wants only the certified set imports coreutils' list.
//
// Deliberately EXCLUDED (like cmds/graph): cmds/foreman and cmds/resources.
// foreman imports pkg/dag, whose tests import this package, so including it
// creates an import cycle. resources imports the AgentOS weave/chat/telemetry
// stack. Both are host front-door verbs rather than bare userland tools; hosts
// register them directly.
//
// Keep the list alphabetical.
package all

import (
	_ "github.com/qiangli/coreutils/cmds/all"

	_ "github.com/qiangli/yoke/cmds/ast"
	_ "github.com/qiangli/yoke/cmds/atq"
	_ "github.com/qiangli/yoke/cmds/atrm"
	_ "github.com/qiangli/yoke/cmds/browser"
	_ "github.com/qiangli/yoke/cmds/cal"
	_ "github.com/qiangli/yoke/cmds/clip"
	_ "github.com/qiangli/yoke/cmds/duration"
	_ "github.com/qiangli/yoke/cmds/fetch"
	_ "github.com/qiangli/yoke/cmds/gzip"
	_ "github.com/qiangli/yoke/cmds/hexdump"
	_ "github.com/qiangli/yoke/cmds/jq"
	_ "github.com/qiangli/yoke/cmds/ntp"
	_ "github.com/qiangli/yoke/cmds/tar"
	_ "github.com/qiangli/yoke/cmds/tokens"
	_ "github.com/qiangli/yoke/cmds/tree"
	_ "github.com/qiangli/yoke/cmds/tz"
	_ "github.com/qiangli/yoke/cmds/watch"
	_ "github.com/qiangli/yoke/cmds/which"
	_ "github.com/qiangli/yoke/cmds/why"
)
