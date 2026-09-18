//go:build embed_vfkit

package vfkit_embed

import _ "embed"

//go:embed vfkit.gz
var embeddedVfkitGz []byte

func init() {
	compressedVfkit = embeddedVfkitGz
}
