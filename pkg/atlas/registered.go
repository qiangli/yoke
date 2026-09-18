// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import (
	"slices"
	"sort"
)

// Registered command kinds — the one thing a registered record's author
// supplies: how the command is implemented.
const (
	RegisteredExec     = "exec"     // an argv template naming an executable the host already has
	RegisteredDownload = "download" // a pinned release binary bashy provisions (binmgr, sha256 in the record)
	RegisteredScript   = "script"   // an inline body run through bashy itself
)

// RegisteredSpec is the atlas-relevant projection of one registered
// command record (bashy's `commands add`). Empty fields take the
// defaults below; the closed vocabularies are the same as every table
// entry's, so a registered command sits in every view beside the shipped
// ones without a special case.
type RegisteredSpec struct {
	Kind    string // exec | download | script
	Group   string // default shellutils
	Tier    string // default userland
	Stage   string // default cross
	Shape   OutputShape
	Caps    []string
	Effects []string // the author's declaration; the mechanism adds nothing here
	OS      []string // default: every platform
	AliasOf string
}

// RegisteredEntry derives the atlas entry for a registered command, the
// way RegistryEntry derives one for a declarative-registry CLI: from data,
// never from a hand-listed table row. The mechanism contributes what it
// knows for certain — an exec'd program spawns a process, a download is
// cached, network-provisioned and self-provisioning — and leaves the
// security effects exactly as the author declared them, because "curated,
// never inferred" holds for a record the operator wrote as much as for one
// bashy shipped. Origin is the exclusive OriginRegistered so every render
// site can tell a registered name from a shipped one.
func RegisteredEntry(s RegisteredSpec) Entry {
	e := Entry{
		Group:    s.Group,
		Tier:     s.Tier,
		Stage:    s.Stage,
		Shape:    NormalizeShape(s.Shape),
		Subclass: SubclassRegistered,
		Origin:   OriginRegistered,
		Posix:    false,
		AliasOf:  s.AliasOf,
		Caps:     append([]string(nil), s.Caps...),
		Effects:  append([]string(nil), s.Effects...),
		OS:       append([]string(nil), s.OS...),
	}
	if e.Group == "" {
		e.Group = GroupShellutils
	}
	if e.Tier == "" {
		e.Tier = TierUserland
	}
	if e.Stage == "" {
		e.Stage = StageCross
	}
	if len(e.OS) == 0 {
		e.OS = OSes()
	}
	switch s.Kind {
	case RegisteredExec:
		e.Caps = append(e.Caps, CapSpawnsProcesses)
	case RegisteredDownload:
		e.Caps = append(e.Caps, CapCached, CapNeedsNetwork, CapSelfProvisioning, CapSpawnsProcesses)
	}
	e.Caps = dedupSorted(e.Caps)
	e.Effects = dedupSorted(e.Effects)
	sort.Strings(e.OS)
	return e
}

// RegisteredKinds returns the closed registered-command kind vocabulary.
func RegisteredKinds() []string {
	return []string{RegisteredDownload, RegisteredExec, RegisteredScript}
}

func dedupSorted(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
