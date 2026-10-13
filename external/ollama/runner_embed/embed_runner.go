//go:build embed_runner

package runner_embed

import _ "embed"

//go:embed ycode-runner.gz
var embeddedRunnerGz []byte

//go:embed ycode-runner.sigstore.json
var embeddedRunnerSignature []byte

func init() {
	compressedRunner = embeddedRunnerGz
	embeddedRunnerBundle = embeddedRunnerSignature
}
