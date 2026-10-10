// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

// RegisterRegistryKind opts a non-fleet registry (skills, herald peers, …)
// into claims with two probes and no per-noun engine code. The kind is a
// plain name-matched kind in its own domain: two claims collide only when
// they name the same entry. A nil exists reports every name absent; a nil
// members, or one that answers empty, claims just the name itself.
func RegisterRegistryKind(kind string, exists func(string) bool, members func(string) ([]string, error)) {
	RegisterProvider(registryKind{kind: kind, exists: exists, members: members})
}

type registryKind struct {
	kind    string
	exists  func(string) bool
	members func(string) ([]string, error)
}

func (r registryKind) Kind() Kind {
	return Kind{Name: r.kind, Match: MatchName, Domain: r.kind}
}

func (r registryKind) Exists(name string) bool {
	if r.exists == nil {
		return false
	}
	return r.exists(name)
}

func (r registryKind) Members(name string) ([]string, error) {
	if r.members != nil {
		if m, err := r.members(name); err != nil || len(m) > 0 {
			return m, err
		}
	}
	return []string{name}, nil
}
