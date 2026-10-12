package fleet

import (
	"embed"
	"fmt"
	"strings"
)

// Optional service units are shipped with the binary but are NOT seeds: they
// never appear in `bashy service list` until a setup skill installs one with
// `bashy service add --optional NAME`, fed from OptionalService. matrix-chat (the
// tuwunel "Matrix chat" homeserver, Sprint 433) is the first.
//
//go:embed optional/*.yaml
var optionalFS embed.FS

// OptionalService returns the raw unit file of a shipped optional service.
func OptionalService(name string) ([]byte, error) {
	b, err := optionalFS.ReadFile("optional/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("fleet: no optional service %q (available: %s)", name, strings.Join(OptionalServiceNames(), ", "))
	}
	return b, nil
}

// OptionalServiceNames lists the shipped optional units, sorted.
func OptionalServiceNames() []string {
	entries, _ := optionalFS.ReadDir("optional")
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, n)
		}
	}
	return names
}
