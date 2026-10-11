// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package identity provides content-addressable and contract-projection identity
// hashing for every bashy unit (dag targets and .bsh scripts), mirroring
// the dual identities of skills (Identity = "h" + sha256 of canonical bytes;
// Capability = "k" + sha256 of the contract projection).
package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/unit/frontmatter"
)

// ErrNoContract reports that a unit declares no contract (Require, Ensure, or Effects).
var ErrNoContract = errors.New("unit: unit declares no contract, so it has no capability key")

// Header is the envelope metadata read from a script's leading comment block.
type Header struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Type        string            `json:"type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// DagTargetIdentity computes the content address ("h" + sha256) of a dag target
// over canonical bytes = the target's heading line + its metadata lines + its body
// (normalizing CRLF to LF). It also updates task.Identity.
func DagTargetIdentity(task *dag.Task) string {
	if task == nil {
		return ""
	}
	id := dag.TargetIdentity(task)
	task.Identity = id
	return id
}

// DagTargetCapability computes the contract projection hash ("k" + sha256) of a dag target
// over its Require/Ensure/Effects contract projection.
// Name, description, prerequisites, body/steps, and placement policy are projected away.
// Returns empty string when the target declares no contract. It also updates task.Capability.
func DagTargetCapability(task *dag.Task) string {
	if task == nil {
		return ""
	}
	cap := dag.TargetCapability(task)
	task.Capability = cap
	return cap
}

// DagTargetCapabilityKey returns DagTargetCapability or ErrNoContract if no contract is declared.
func DagTargetCapabilityKey(task *dag.Task) (string, error) {
	cap := DagTargetCapability(task)
	if cap == "" {
		return "", ErrNoContract
	}
	return cap, nil
}

// ScriptIdentity computes the content address ("h" + sha256) of a .bsh script
// over whole file bytes with CRLF normalized to LF.
func ScriptIdentity(src []byte) string {
	norm := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(norm)
	return "h" + hex.EncodeToString(sum[:])
}

// ScriptHeader reads the envelope (name, description, type) from a script's
// leading '#' comment block (skipping a '#!' shebang line; lines like
// '# name: x', '# description: y', '# type: z'; stopping at the first non-comment line).
func ScriptHeader(src []byte) Header {
	var h Header
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	idx := 0

	// Skip leading empty lines before shebang
	for idx < len(lines) && strings.TrimSpace(lines[idx]) == "" {
		idx++
	}

	// Skip a '#!' shebang line
	if idx < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[idx]), "#!") {
		idx++
		// Skip optional blank lines between shebang and comment block
		for idx < len(lines) && strings.TrimSpace(lines[idx]) == "" {
			idx++
		}
	}

	// If the first line is not a '#' comment, there is no leading comment block.
	if idx >= len(lines) || !isCommentLine(lines[idx]) {
		return h
	}

	for ; idx < len(lines); idx++ {
		line := lines[idx]
		if !isCommentLine(line) {
			break // stop at first non-comment line
		}
		trimmed := strings.TrimSpace(line)
		content := strings.TrimPrefix(trimmed, "#")
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if k, v, ok := frontmatter.LineKV(content); ok {
			switch k {
			case "name":
				h.Name = v
			case "description":
				if h.Description == "" {
					h.Description = v
				} else {
					h.Description += "\n" + v
				}
			case "type":
				h.Type = v
			default:
				if h.Metadata == nil {
					h.Metadata = make(map[string]string)
				}
				h.Metadata[k] = v
			}
		}
	}
	return h
}

func isCommentLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "#")
}
