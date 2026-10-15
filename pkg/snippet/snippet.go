// Package snippet runs code embedded in configuration under a declared effect cap.
//
// Configuration seeds should route agentic probes through the bashy door and
// default them to a local or free model. Inferred-result cache lifetimes belong
// to callers; the probe engine uses a one-hour TTL.
package snippet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

const (
	ClassMeasured = "measured"
	ClassInferred = "inferred"
)

type Snippet struct {
	Lang    string            `json:"lang,omitempty" yaml:"lang,omitempty" doc:"snippet language"`
	Body    string            `json:"body" yaml:"body" doc:"Bash# source code"`
	Effects []string          `json:"effects,omitempty" yaml:"effects,omitempty" doc:"declared effects"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty" doc:"configuration credential references"`
	Timeout time.Duration     `json:"-" yaml:"-"`
}

type snippetWire struct {
	Lang    string            `json:"lang,omitempty" yaml:"lang,omitempty"`
	Body    string            `json:"body" yaml:"body"`
	Effects []string          `json:"effects,omitempty" yaml:"effects,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Timeout string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

func (s Snippet) short() bool {
	return (s.Lang == "" || s.Lang == "bsh") && len(s.Effects) == 0 && len(s.Env) == 0 && s.Timeout == 0
}
func (s Snippet) wire() snippetWire {
	return snippetWire{s.Lang, s.Body, s.Effects, s.Env, durationString(s.Timeout)}
}
func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
func (s *Snippet) fromWire(w snippetWire) error {
	d, err := time.ParseDuration(w.Timeout)
	if w.Timeout == "" {
		d, err = 0, nil
	}
	if err != nil {
		return fmt.Errorf("snippet timeout: %w", err)
	}
	*s = Snippet{w.Lang, w.Body, w.Effects, w.Env, d}
	return nil
}
func (s Snippet) MarshalJSON() ([]byte, error) {
	if s.short() {
		return json.Marshal(s.Body)
	}
	return json.Marshal(s.wire())
}
func (s *Snippet) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var body string
		if err := json.Unmarshal(b, &body); err != nil {
			return err
		}
		*s = Snippet{Lang: "bsh", Body: body}
		return nil
	}
	var w snippetWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	return s.fromWire(w)
}
func (s Snippet) MarshalYAML() (any, error) {
	if s.short() {
		return s.Body, nil
	}
	return s.wire(), nil
}
func (s *Snippet) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var body string
		if err := n.Decode(&body); err != nil {
			return err
		}
		*s = Snippet{Lang: "bsh", Body: body}
		return nil
	}
	var w snippetWire
	if err := n.Decode(&w); err != nil {
		return err
	}
	return s.fromWire(w)
}

type IO struct {
	Dir       string
	Env       []string
	Stdin     io.Reader
	MaxStdout int64
}
type Result struct {
	Stdout         []byte
	Stderr         []byte
	Exit           int
	Refused        []string
	Classification string
}
type Runner func(context.Context, Snippet, IO) (Result, error)
type CredentialResolver func(context.Context, string) (string, error)
type ModelRef struct{ Provider, Model, Plan string }

var (
	ErrNotConfigured     = errors.New("snippet: credential not configured")
	ErrUndeclaredAgentic = errors.New("snippet: agentic execution requires spend effect")
	ErrNeedsInput        = errors.New("snippet: input required")
	ErrSelfMeasure       = errors.New("snippet: agent cannot measure its own plan")
)

var hooks struct {
	sync.RWMutex
	runner    Runner
	secret    CredentialResolver
	model     func(context.Context, ModelRef) (string, error)
	agentPlan func(agent string) (plan string, ok bool)
}

func SetRunner(r Runner)                     { hooks.Lock(); defer hooks.Unlock(); hooks.runner = r }
func SetSecretResolver(r CredentialResolver) { hooks.Lock(); defer hooks.Unlock(); hooks.secret = r }
func SetModelCredentialResolver(r func(context.Context, ModelRef) (string, error)) {
	hooks.Lock()
	defer hooks.Unlock()
	hooks.model = r
}
func SetAgentPlanResolver(r func(agent string) (plan string, ok bool)) {
	hooks.Lock()
	defer hooks.Unlock()
	hooks.agentPlan = r
}

type modelKey struct{}
type measuringKey struct{}

func WithModel(ctx context.Context, provider, model, plan string) context.Context {
	return context.WithValue(ctx, modelKey{}, ModelRef{provider, model, plan})
}

// WithMeasuring records the plan whose usage the snippet is measuring.
func WithMeasuring(ctx context.Context, plan string) context.Context {
	return context.WithValue(ctx, measuringKey{}, plan)
}

// IsAgentic reports whether the Bash# parser recognizes an agentic block or
// agentic function declaration. Invalid programs and ordinary commands or
// quoted words named "agentic" are not agentic snippets.
func IsAgentic(s Snippet) bool {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(s.Body), "snippet.bsh")
	if err != nil {
		return false
	}
	seen := false
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BashPPAgenticBlock:
			seen = true
		case *syntax.FuncDecl:
			if n.Agentic != nil {
				seen = true
			}
		case *syntax.BashPPFuncDecl:
			if n.Agentic != nil {
				seen = true
			}
		}
		return !seen
	})
	return seen
}

func CredentialPathRefused(body string) (string, bool) {
	for _, p := range []string{"auth.json", ".credentials.json", "oauth_creds.json", "credentials.json", "Library/Keychains", ".netrc"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(p)) {
			return p, true
		}
	}
	return "", false
}
func NeedsConsent(s Snippet) bool {
	for _, e := range s.Effects {
		if e != "pure" && e != "read" {
			return true
		}
	}
	return false
}

func Run(ctx context.Context, s Snippet, tio IO) (Result, error) {
	agentic := IsAgentic(s)
	classification := ClassMeasured
	if agentic {
		classification = ClassInferred
	}
	zero := Result{Classification: classification}
	lang := s.Lang
	if lang == "" {
		lang = "bsh"
	}
	switch lang {
	case "bsh", "bashsharp", "bash", "sh":
	default:
		return zero, fmt.Errorf("snippet: unknown language %q", lang)
	}
	s.Lang = lang
	valid := map[string]bool{}
	for _, e := range atlas.Effects() {
		valid[e] = true
	}
	for _, e := range s.Effects {
		if !valid[e] {
			return zero, fmt.Errorf("snippet: unknown effect %q", e)
		}
	}
	if agentic && !hasEffect(s.Effects, "spend") {
		return zero, ErrUndeclaredAgentic
	}
	if len(s.Effects) == 0 {
		s.Effects = []string{"pure"}
	}
	for _, effect := range s.Effects {
		if effect == "read" {
			if p, ok := CredentialPathRefused(s.Body); ok {
				return zero, fmt.Errorf("snippet: credential path %q refused", p)
			}
			break
		}
	}
	if s.Timeout < 0 {
		return zero, fmt.Errorf("snippet: negative timeout")
	}
	if s.Timeout == 0 {
		s.Timeout = 30 * time.Second
	}
	if tio.MaxStdout <= 0 {
		tio.MaxStdout = 1 << 20
	}
	if tio.Env == nil {
		tio.Env = os.Environ()
	}
	hooks.RLock()
	runner, secret, model, agentPlan := hooks.runner, hooks.secret, hooks.model, hooks.agentPlan
	hooks.RUnlock()
	if measuredPlan, measuring := ctx.Value(measuringKey{}).(string); measuring && agentPlan != nil {
		for _, agent := range targetedAgents(s) {
			if plan, ok := agentPlan(agent); ok && plan == measuredPlan {
				return zero, fmt.Errorf("%w: agent %q uses plan %q", ErrSelfMeasure, agent, plan)
			}
		}
	}
	if runner == nil {
		return zero, errors.New("snippet: no runner registered")
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	secrets := []string{}
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return zero, fmt.Errorf("snippet: invalid environment name %q", k)
		}
		ref := s.Env[k]
		var value string
		var err error
		switch {
		case strings.HasPrefix(ref, "secret:"):
			if secret == nil {
				err = ErrNotConfigured
			} else {
				value, err = secret(ctx, strings.TrimPrefix(ref, "secret:"))
			}
		case strings.HasPrefix(ref, "env:"):
			var ok bool
			value, ok = os.LookupEnv(strings.TrimPrefix(ref, "env:"))
			if !ok || value == "" {
				err = ErrNotConfigured
			}
		case ref == "model:credential":
			m, ok := ctx.Value(modelKey{}).(ModelRef)
			if !ok || model == nil {
				err = ErrNotConfigured
			} else {
				value, err = model(ctx, m)
			}
		default:
			return zero, fmt.Errorf("snippet: literal environment value for %q refused", k)
		}
		if err != nil {
			return zero, fmt.Errorf("snippet: resolve %s: %w", k, ErrNotConfigured)
		}
		if value == "" {
			return zero, fmt.Errorf("snippet: resolve %s: %w", k, ErrNotConfigured)
		}
		tio.Env = append(tio.Env, k+"="+value)
		secrets = append(secrets, value)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	r, err := runner(ctx, s, tio)
	r.Classification = classification
	if int64(len(r.Stdout)) > tio.MaxStdout {
		r.Stdout = r.Stdout[:tio.MaxStdout]
		err = errors.Join(err, fmt.Errorf("snippet: stdout exceeds %d bytes", tio.MaxStdout))
	}
	for _, v := range secrets {
		r.Stderr = bytes.ReplaceAll(r.Stderr, []byte(v), []byte("***"))
		if err != nil {
			err = errors.New(strings.ReplaceAll(err.Error(), v, "***"))
		}
	}
	if r.Exit == 6 {
		if text := yieldText(r.Stdout, r.Stderr); text != "" {
			return r, fmt.Errorf("%w: %s", ErrNeedsInput, text)
		}
		return r, ErrNeedsInput
	}
	return r, err
}

func hasEffect(effects []string, want string) bool {
	for _, effect := range effects {
		if effect == want {
			return true
		}
	}
	return false
}

// targetedAgents recognizes the deliberately small self-measurement surface:
// whitespace-delimited "@agent NAME", "bashy agent run NAME", and
// "--agent NAME" tokens in the body or environment keys and references.
func targetedAgents(s Snippet) []string {
	sources := []string{s.Body}
	keys := make([]string, 0, len(s.Env))
	for key := range s.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sources = append(sources, key, s.Env[key])
	}
	var agents []string
	for _, source := range sources {
		fields := strings.Fields(source)
		for i := range fields {
			switch {
			case fields[i] == "@agent" && i+1 < len(fields):
				agents = append(agents, cleanAgentToken(fields[i+1]))
			case fields[i] == "bashy" && i+3 < len(fields) && fields[i+1] == "agent" && fields[i+2] == "run":
				agents = append(agents, cleanAgentToken(fields[i+3]))
			case fields[i] == "--agent" && i+1 < len(fields):
				agents = append(agents, cleanAgentToken(fields[i+1]))
			}
		}
	}
	return agents
}

func cleanAgentToken(s string) string {
	return strings.Trim(s, "'\"`;,(){}[]")
}

func yieldText(stdout, stderr []byte) string {
	const limit = 2 << 10
	combined := make([]byte, 0, min(limit, len(stdout)+len(stderr)+1))
	appendPart := func(part []byte) {
		if len(part) == 0 || len(combined) == limit {
			return
		}
		if len(combined) > 0 {
			combined = append(combined, '\n')
		}
		remaining := limit - len(combined)
		combined = append(combined, part[:min(remaining, len(part))]...)
	}
	appendPart(stdout)
	appendPart(stderr)
	return string(bytes.TrimSpace(combined))
}
