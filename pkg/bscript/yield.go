// Package bscript contains the bounded command/script lowering used when an
// agentic action cannot proceed without caller input. It does not execute,
// persist, retry, or call a model.
package bscript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/recall"
	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/skills"
)

const (
	SchemaVersion        = "bashy-agentic-yield-v1"
	StatusInputRequired  = "input_required"
	defaultContextBudget = 700
)

// Kind is the only action classification this package understands.
type Kind string

const (
	Command Kind = "command"
	Script  Kind = "script"
)

// Request is a failed action to lower. Exactly one of Args or Source is used,
// according to Kind. Readers are explicit so hermetic callers do not
// accidentally open operator stores; nil means no recall rings.
type Request struct {
	Kind     Kind
	Args     []string
	Source   string
	Readers  []recall.Reader
	Scrubber *redact.Scrubber
}

// Yield is returned to the caller and has no write or execution method. The
// skill record is shareable; Context remains a separate, host-local envelope.
type Yield struct {
	SchemaVersion string               `json:"schema_version" yaml:"schema_version"`
	Status        string               `json:"status" yaml:"status"`
	Skill         skills.Record        `json:"skill" yaml:"skill"`
	Context       recall.ContextResult `json:"context" yaml:"context"`
}

// Lower deterministically turns a failed command or script into a no-contract
// skill proposal. The command is stored in SKILL.md metadata.step-run so an
// explicit `skill add` round-trip cannot lose it.
func Lower(req Request) (Yield, error) {
	run, err := commandText(req)
	if err != nil {
		return Yield{}, err
	}
	scrub := req.Scrubber
	if scrub == nil {
		scrub = redact.FromHost()
	}
	symbolic := scrub.String(run)
	sum := sha256.Sum256([]byte(string(req.Kind) + "\x00" + symbolic))
	name := "agentic-" + hex.EncodeToString(sum[:6])
	description := "Complete a failed " + string(req.Kind) + " that requires caller input."
	md := skillMarkdown(name, description, symbolic)
	rec := skills.Record{
		Name: name, Kind: skills.RecordKind, Description: description,
		Bindings: map[string]string{"step-run": symbolic},
		Files:    map[string]string{"SKILL.md": md},
	}
	if err := skills.ValidateRecordShareable(rec, scrub); err != nil {
		return Yield{}, err
	}
	ctx := recall.Context(recall.Query{
		Text: symbolic, K: 2, Budget: defaultContextBudget,
	}, req.Readers...)
	return Yield{
		SchemaVersion: SchemaVersion,
		Status:        StatusInputRequired,
		Skill:         rec,
		Context:       ctx,
	}, nil
}

func commandText(req Request) (string, error) {
	switch req.Kind {
	case Command:
		if len(req.Args) == 0 || strings.TrimSpace(req.Args[0]) == "" {
			return "", errors.New("bscript: command has no argv")
		}
		parts := make([]string, len(req.Args))
		for i, arg := range req.Args {
			parts[i] = shellQuote(arg)
		}
		return strings.Join(parts, " "), nil
	case Script:
		if len(req.Args) > 0 {
			parts := make([]string, len(req.Args)+1)
			parts[0] = "bashy"
			for i, arg := range req.Args {
				parts[i+1] = shellQuote(arg)
			}
			return strings.Join(parts, " "), nil
		}
		if strings.TrimSpace(req.Source) == "" {
			return "", errors.New("bscript: script has no path or source")
		}
		return "bashy -c " + shellQuote(req.Source), nil
	default:
		return "", fmt.Errorf("bscript: unsupported action kind %q", req.Kind)
	}
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func skillMarkdown(name, description, run string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  step-run: %q\n---\n\n# Caller input required\n\nReview, complete, and contract this action before running it.\n", name, description, run)
}
