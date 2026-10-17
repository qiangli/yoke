package fleet

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// Revision hashes the model, agent and tool ring contents without parsing or
// resolving bindings. It includes every ring, so removing an override or
// changing a shared/cloud record is visible too. Catalog consumers can poll
// this cheaply instead of repeatedly deriving the complete binding table.
func (c *Catalog) Revision() ([32]byte, error) {
	h := sha256.New()
	for _, noun := range []string{dirTools, dirModels, dirAgents} {
		for i, src := range c.sources(noun) {
			names, err := src.Names()
			if err != nil {
				return [32]byte{}, err
			}
			sort.Strings(names)
			for _, name := range names {
				body, ok := src.Body(name)
				if !ok {
					return [32]byte{}, fmt.Errorf("fleet: cannot read %s/%s", noun, name)
				}
				fmt.Fprintf(h, "%s:%d:%d:%s:%d:", noun, i, len(name), name, len(body))
				h.Write(body)
			}
		}
	}
	var revision [32]byte
	copy(revision[:], h.Sum(nil))
	return revision, nil
}
