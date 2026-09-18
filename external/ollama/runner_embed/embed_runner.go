//go:build embed_runner

package runner_embed

import _ "embed"

//go:embed ycode-runner.gz
var embeddedRunnerGz []byte

func init() {
	compressedRunner = embeddedRunnerGz
}
