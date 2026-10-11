// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
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

// TargetCapability computes the target's contract projection hash ("k" + sha256)
// over its Require/Ensure/Effects contract projection.
// Name, description, prerequisites, body/steps, and placement policy are
// projected away. Returns empty string when the target declares no contract.
func TargetCapability(t *Task) string {
	if t == nil {
		return ""
	}
	req := normaliseList(t.Require)
	ens := normaliseList(t.Ensure)
	eff := normaliseList(t.Effects)
	if len(req) == 0 && len(ens) == 0 && len(eff) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range req {
		b.WriteString("require: ")
		b.WriteString(r)
		b.WriteByte('\n')
	}
	for _, e := range ens {
		b.WriteString("ensure: ")
		b.WriteString(e)
		b.WriteByte('\n')
	}
	for _, ef := range eff {
		b.WriteString("effects: ")
		b.WriteString(ef)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "k" + hex.EncodeToString(sum[:])
}

func normaliseList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
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
