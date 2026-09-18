//go:build embed_podman

package podman_embed

import _ "embed"

//go:embed podman.gz
var embeddedPodmanGz []byte

func init() {
	compressedPodman = embeddedPodmanGz
}
