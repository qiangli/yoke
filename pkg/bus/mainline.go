// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package bus

import (
	"regexp"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// IsMainLineTool reports whether name resolves to a registered steerable tool
// main line (e.g. claude, codex) even when no instance is currently open.
// Injected by the host alongside FleetNames/FleetResolveName.
var IsMainLineTool func(string) bool

var plusExtensionPattern = regexp.MustCompile(`^(.*[a-zA-Z0-9].*)\+([0-9]+)$`)

// ParseExtensionLabel parses a direct extension address of the form NAME+N
// (digits only after '+'). It returns the base main-line name, the numeric
// extension suffix, and true when matched.
func ParseExtensionLabel(s string) (base, ext string, ok bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	m := plusExtensionPattern.FindStringSubmatch(s)
	if m == nil {
		return s, "", false
	}
	return strings.TrimSpace(m[1]), m[2], true
}

// MainLineBase returns the main-line name for an instance label (NAME+N -> NAME,
// NAME -> NAME).
func MainLineBase(label string) string {
	base, _, _ := ParseExtensionLabel(label)
	return strings.TrimSpace(base)
}

// BoardHeadSeq returns the current highest sequence on the message board (0
// when empty), suitable for computing a fresh instance's MailFrom = head + 1.
func BoardHeadSeq() int64 {
	posts, err := Posts()
	head := archivedThrough()
	if err == nil && len(posts) > 0 && posts[len(posts)-1].Seq > head {
		head = posts[len(posts)-1].Seq
	}
	return head
}

// ClaimMatchesReader reports whether a ClaimHolder value belongs to reader.
// Both direct strings (e.g. "NAME", "NAME+2") and instance addresses
// ("instance/<uuid>", "<uuid>") are matched against the reader's instance UUID
// or label.
func ClaimMatchesReader(holder, reader string) bool {
	h := strings.TrimSpace(holder)
	r := strings.TrimSpace(reader)
	if h == "" || r == "" {
		return false
	}
	if strings.EqualFold(h, r) {
		return true
	}
	if hid, hok := ExplicitInstanceID(h); hok {
		if rid, rok := ExplicitInstanceID(r); rok {
			return strings.EqualFold(hid, rid)
		}
	}
	return false
}

// ReaderHasLiveSession reports whether reader has an active, live session card
// in the host room proving it is eligible to claim a hunt-group post on line.
func ReaderHasLiveSession(reader, line string) bool {
	reader = strings.TrimSpace(reader)
	line = strings.TrimSpace(line)
	if reader == "" || line == "" {
		return false
	}
	if id, ok := ExplicitInstanceID(reader); ok {
		inst, err := InstanceStoreFn().Get(id)
		if err != nil || !inst.Active() {
			return false
		}
		if !instanceBelongsToLine(inst, line) {
			return false
		}
		return instanceCardLive(inst)
	}
	if inst, found, err := ResolveLiveInstanceByLabel(reader); err == nil && found {
		if !instanceBelongsToLine(inst, line) {
			return false
		}
		return instanceCardLive(inst)
	}
	if !strings.EqualFold(MainLineBase(reader), line) && !strings.EqualFold(resolveBoardName(reader), line) {
		return false
	}
	return namedCardLive(reader)
}

func instanceBelongsToLine(inst fleet.Instance, line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.EqualFold(MainLineBase(inst.Label), line) {
		return true
	}
	if inst.Handle != "" && strings.EqualFold(MainLineBase(inst.Handle), line) {
		return true
	}
	if canon := strings.TrimSpace(resolveBoardName(line)); canon != "" {
		if strings.EqualFold(MainLineBase(inst.Label), canon) {
			return true
		}
	}
	return false
}

func instanceCardLive(inst fleet.Instance) bool {
	if _, live, err := room.Find(inst.ClaimID()); err == nil && live {
		return true
	}
	if label := strings.TrimSpace(inst.Label); label != "" {
		if _, live, err := room.Find(room.AgentClaimID(label)); err == nil && live {
			return true
		}
		if _, live, err := room.Find(label); err == nil && live {
			return true
		}
	}
	return false
}

func namedCardLive(name string) bool {
	if _, live, err := room.Find(room.AgentClaimID(name)); err == nil && live {
		return true
	}
	if _, live, err := room.Find(name); err == nil && live {
		return true
	}
	return false
}

// readerMatchesMainLine reports whether reader is an eligible member of the
// hunt-group main line (used for filtering/visibility; claiming still requires
// ReaderHasLiveSession).
func readerMatchesMainLine(reader, line string) bool {
	reader = strings.TrimSpace(reader)
	line = strings.TrimSpace(line)
	if reader == "" || line == "" {
		return false
	}
	if strings.EqualFold(reader, line) {
		return true
	}
	if id, ok := ExplicitInstanceID(reader); ok {
		inst, err := InstanceStoreFn().Get(id)
		if err != nil || !inst.Active() {
			return false
		}
		return instanceBelongsToLine(inst, line)
	}
	return strings.EqualFold(MainLineBase(reader), line)
}

// hasPlusExtensions reports whether any active instance in all holds a NAME+N
// label under line.
func hasPlusExtensions(line string, all []fleet.Instance) bool {
	for _, i := range all {
		if !i.Active() {
			continue
		}
		if base, _, ok := ParseExtensionLabel(i.Label); ok && strings.EqualFold(base, line) {
			return true
		}
		if base, _, ok := ParseExtensionLabel(i.Handle); ok && strings.EqualFold(base, line) {
			return true
		}
	}
	return false
}

// hasNonLineFamilyAmbiguity reports whether line is a family name with live
// non-extension instances (e.g. Ada/Bea on esme, or Esme/Esme-2 on
// claude-opus5.5) that must retain ambiguity/label-resolution behavior.
func hasNonLineFamilyAmbiguity(line string, all []fleet.Instance) bool {
	canon := strings.ToLower(resolveBoardName(line))
	want := strings.ToLower(strings.TrimSpace(line))
	var nonLineLive int
	for _, i := range all {
		if !i.Active() {
			continue
		}
		f := strings.ToLower(strings.TrimSpace(i.Family))
		if f == "" || (f != want && f != canon && !strings.EqualFold(i.FamilyID, line)) {
			continue
		}
		if !instanceBelongsToLine(i, line) {
			nonLineLive++
		}
	}
	return nonLineLive >= 2
}
