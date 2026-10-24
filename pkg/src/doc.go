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
//	src branch       — topic branches off the host's own local branch.
//	src status       — branch pins plus pristine|modified|modified-protected.
//	src diff/log     — review committed divergence and history.
//	src pr           — local pull requests (open, show, list, merge, close).
//	src patch        — move changes between hosts by file.
//	src upgrade      — re-pin pristine and rebase local onto the release.
//	src revert       — reset the local checkout to pristine (dry-run default).
package src
