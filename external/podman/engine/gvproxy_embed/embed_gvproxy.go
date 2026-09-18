//go:build embed_gvproxy

package gvproxy_embed

import _ "embed"

//go:embed gvproxy.gz
var embeddedGvproxyGz []byte

func init() {
	compressedGvproxy = embeddedGvproxyGz
}
