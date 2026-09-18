package weave

import "io"

// ProvisionWorkspace, when set by the host binary, provisions a freshly
// created issue workspace before any agent launches in it — e.g. bashy
// wires it to the skills catalog so every workspace carries the agent
// skill surface (.agents/skills + .claude/skills). A hook var, not an
// import: pkg/weave stays dependency-lean (pure filesystem), and hosts
// that don't care simply leave it nil.
var ProvisionWorkspace func(workspace string, stderr io.Writer)
