package fleet

import (
	"embed"
	"fmt"
)

// Optional service units are shipped with the binary but are NOT seeds: they
// never appear in `bashy service list` until a setup skill installs one with
// `bashy service add -f -`, fed from OptionalService. matrix-chat (the
// tuwunel "Matrix chat" homeserver, Sprint 433) is the first.
//
//go:embed optional/*.yaml
var optionalFS embed.FS

// OptionalService returns the raw unit file of a shipped optional service.
func OptionalService(name string) ([]byte, error) {
	b, err := optionalFS.ReadFile("optional/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("fleet: no optional service %q", name)
	}
	return b, nil
}
