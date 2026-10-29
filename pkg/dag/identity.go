// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/qiangli/yoke/pkg/unit"
)

// TargetIdentity computes the target's canonical identity hash ("h" + sha256)
// over canonical bytes = the target's heading line + its metadata lines + its body.
// All CRLF sequences are normalized to LF.
func TargetIdentity(t *Task) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	heading := strings.TrimRight(t.Heading, "\r\n")
	if heading == "" && t.Name != "" {
		heading = "## " + t.Name
	}
	if heading != "" {
		b.WriteString(heading)
		b.WriteByte('\n')
	}
	for _, ml := range t.MetaLines {
		ml = strings.TrimRight(ml, "\r\n")
		if ml != "" {
			b.WriteString(ml)
			b.WriteByte('\n')
		}
	}
	if t.Body != "" {
		b.WriteString(t.Body)
	}
	canon := strings.ReplaceAll(b.String(), "\r\n", "\n")
	sum := sha256.Sum256([]byte(canon))
	return "h" + hex.EncodeToString(sum[:])
}

// TargetCapability computes the target's capability key ("k" + sha256)
// through the one unit formula (unit.CapabilityKey) over its Require/Ensure/
// Effects contract. Name, description, prerequisites, body/steps, and
// placement policy are projected away. Returns empty string when the target
// is uncontracted (no Require and no Ensure).
func TargetCapability(t *Task) string {
	if t == nil {
		return ""
	}
	key, _ := unit.CapabilityKey(unit.Contract{Require: t.Require, Ensure: t.Ensure, Effects: t.Effects})
	return key
}

// TargetIdentity returns the target's canonical identity hash, recomputing
// and updating t.Identity if t was modified.
func (t *Task) TargetIdentity() string {
	if t == nil {
		return ""
	}
	id := TargetIdentity(t)
	t.Identity = id
	return id
}

// TargetCapability returns the target's contract projection hash, recomputing
// and updating t.Capability if t was modified.
func (t *Task) TargetCapability() string {
	if t == nil {
		return ""
	}
	cap := TargetCapability(t)
	t.Capability = cap
	return cap
}
