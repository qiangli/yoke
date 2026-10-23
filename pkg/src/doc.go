// Package src is the `src` source-provenance front door (Sprint 458 M1,
// yoke half): the class axis, the command-to-source map, the pinned OSS
// fetch, and the protected-tree check behind `bashy src`.
//
// bashy mounts the rod from NewSrcCmd (M1b wires it); this package owns the
// policy and the git mechanics. It is stdlib + cobra + go-git only, through
// yoke's pure-Go git client (github.com/qiangli/yoke/git) — no shell-outs.
//
// Subcommands:
//
//	src show <cmd>   — repo, Go package path, class, release commit.
//	src fetch [repo] — pin $BASHY_HOME/src/<repo> at the release commit.
//	src check        — flag working-tree edits under core/protected paths.
package src
