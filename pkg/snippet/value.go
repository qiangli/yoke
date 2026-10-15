package snippet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Value is a configuration value that is either a literal or Bash# code.
// In YAML a node tagged !bsh holds code (a scalar is the body, a mapping
// is the long Snippet form); any other node is a literal decoded into T.
// In JSON {"$bsh": "body"} (or the long object form) is code. Code is
// never run by Unmarshal — only Resolve runs it, lazily.
type Value[T any] struct {
	Lit  T
	Code *Snippet
	set  bool
}

// IsCode reports whether the value holds code rather than a literal.
func (v Value[T]) IsCode() bool { return v.Code != nil }

// ErrConsentRequired is returned by Resolve when the code declares
// effects that need consent and the context does not carry it.
var ErrConsentRequired = errors.New("snippet: consent required")

type consentKey struct{}

// WithConsent marks a context as carrying the operator's consent to run
// snippets that declare effects beyond pure and read.
func WithConsent(ctx context.Context) context.Context {
	return context.WithValue(ctx, consentKey{}, true)
}

// Resolve returns the literal, or runs the code and parses its trimmed
// stdout as YAML into T (YAML is a JSON superset, so JSON output parses
// the same way).
func (v Value[T]) Resolve(ctx context.Context) (T, error) {
	var zero T
	if v.Code == nil {
		return v.Lit, nil
	}
	s := *v.Code
	if len(s.Effects) == 0 {
		s.Effects = []string{"pure"}
	}
	if NeedsConsent(s) {
		if _, ok := ctx.Value(consentKey{}).(bool); !ok {
			return zero, ErrConsentRequired
		}
	}
	r, err := Run(ctx, s, IO{})
	if err != nil {
		return zero, err
	}
	if len(r.Refused) > 0 {
		return zero, fmt.Errorf("snippet: effects refused: %s", strings.Join(r.Refused, ", "))
	}
	if r.Exit != 0 {
		return zero, fmt.Errorf("snippet: exit %d: %s", r.Exit, strings.TrimSpace(string(r.Stderr)))
	}
	var out T
	if err := yaml.Unmarshal(bytes.TrimSpace(r.Stdout), &out); err != nil {
		return zero, fmt.Errorf("snippet: parse output: %w", err)
	}
	return out, nil
}

const bshTag = "!bsh"

func (v Value[T]) MarshalYAML() (any, error) {
	if v.Code == nil {
		return v.Lit, nil
	}
	if v.Code.short() {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: bshTag, Value: v.Code.Body}, nil
	}
	var n yaml.Node
	if err := n.Encode(v.Code.wire()); err != nil {
		return nil, err
	}
	n.Tag = bshTag
	return &n, nil
}

func (v *Value[T]) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == bshTag {
		s, err := snippetFromNode(n)
		if err != nil {
			return err
		}
		v.Code, v.set = s, true
		return nil
	}
	var lit T
	if err := n.Decode(&lit); err != nil {
		return err
	}
	v.Lit, v.set = lit, true
	return nil
}

func snippetFromNode(n *yaml.Node) (*Snippet, error) {
	if n.Kind == yaml.ScalarNode {
		return &Snippet{Lang: "bsh", Body: n.Value}, nil
	}
	var w snippetWire
	if err := n.Decode(&w); err != nil {
		return nil, err
	}
	var s Snippet
	if err := s.fromWire(w); err != nil {
		return nil, err
	}
	return &s, nil
}

const bshKey = "$bsh"

func (v Value[T]) MarshalJSON() ([]byte, error) {
	if v.Code == nil {
		return json.Marshal(v.Lit)
	}
	body := any(v.Code.Body)
	if !v.Code.short() {
		body = v.Code.wire()
	}
	return json.Marshal(map[string]any{bshKey: body})
}

func (v *Value[T]) UnmarshalJSON(b []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err == nil {
		if raw, ok := probe[bshKey]; ok {
			var body string
			if err := json.Unmarshal(raw, &body); err == nil {
				v.Code, v.set = &Snippet{Lang: "bsh", Body: body}, true
				return nil
			}
			var w snippetWire
			if err := json.Unmarshal(raw, &w); err != nil {
				return err
			}
			s, err := snippetFromWire(w)
			if err != nil {
				return err
			}
			v.Code, v.set = s, true
			return nil
		}
	}
	var lit T
	if err := json.Unmarshal(b, &lit); err != nil {
		return err
	}
	v.Lit, v.set = lit, true
	return nil
}

func snippetFromWire(w snippetWire) (*Snippet, error) {
	var s Snippet
	if err := s.fromWire(w); err != nil {
		return nil, err
	}
	return &s, nil
}
