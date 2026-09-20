package weave

import "github.com/qiangli/yoke/external/gitscm"

// gitBin is the git executable weave execs — the one bashy provisions (MinGit
// on Windows) or the platform git, never an assumption about PATH. A seam so
// tests can point it at a fake. See gitscm.Path for the bug this closes.
var gitBin = gitscm.Path
