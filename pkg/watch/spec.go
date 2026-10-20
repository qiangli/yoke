package watch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Duration is a time.Duration spelled the way a human writes one ("14m").
type Duration time.Duration

// UnmarshalJSON accepts "14m" or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("watch: duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("watch: duration must be a string like \"14m\" or a number of seconds")
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON writes the human spelling back.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D is the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// SourceSpec declares one source in a rule file.
type SourceSpec struct {
	Type    string `json:"type"`
	Locator string `json:"locator,omitempty"`
	// Target narrows a source to one object (a sprint id, a seat, a path).
	Target string `json:"target,omitempty"`
	// Backfill reads records already present that are younger than this, so a
	// watcher started mid-flight can still track a conversation that is
	// already in progress. Zero means "baseline only": start from the head and
	// report nothing that happened before you were asked to watch.
	Backfill   Duration `json:"backfill,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	Command    []string `json:"command,omitempty"`
	AllowEmpty bool     `json:"allow_empty,omitempty"`
	// Reader is the cursor identity a receipt level is measured against.
	Reader string `json:"reader,omitempty"`
}

// RuleSpec declares one rule in a rule file. The fields a type does not use
// are ignored, so the vocabulary reads the same in every rule.
type RuleSpec struct {
	Type   string   `json:"type"`
	Name   string   `json:"name,omitempty"`
	When   Criteria `json:"when,omitempty"`
	Fields []string `json:"fields,omitempty"`

	Within    Duration  `json:"within,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Activity  *Criteria `json:"activity,omitempty"`
	Scope     *Criteria `json:"scope,omitempty"`
	LocatorBy string    `json:"locator_by,omitempty"`
	Expect    []string  `json:"expect,omitempty"`
	NoClear   bool      `json:"no_clear,omitempty"`
	Message   string    `json:"message,omitempty"`

	Trigger *Criteria `json:"trigger,omitempty"`
	Join    string    `json:"join,omitempty"`
	Key     string    `json:"key,omitempty"`
}

// RuleFile is a rule set: a preset, or anything --rules points at.
type RuleFile struct {
	SchemaVersion string       `json:"schema_version"`
	Name          string       `json:"name,omitempty"`
	Description   string       `json:"description,omitempty"`
	Interval      Duration     `json:"interval,omitempty"`
	Renotify      Duration     `json:"renotify,omitempty"`
	Timeout       Duration     `json:"timeout,omitempty"`
	Sources       []SourceSpec `json:"sources,omitempty"`
	Rules         []RuleSpec   `json:"rules,omitempty"`
}

// ParseRuleFile decodes a rule file. An unknown schema version is an error:
// silently reading a future rule file with today's vocabulary would watch
// something other than what it says.
func ParseRuleFile(b []byte) (RuleFile, error) {
	var f RuleFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return RuleFile{}, fmt.Errorf("watch: rule file: %w", err)
	}
	if f.SchemaVersion != "" && f.SchemaVersion != RulesSchemaVersion {
		return RuleFile{}, fmt.Errorf("watch: rule file schema %q, want %q", f.SchemaVersion, RulesSchemaVersion)
	}
	return f, nil
}

// Plan is a built rule file: everything an engine needs but the sinks.
type Plan struct {
	Sources  []Source
	Rules    []Rule
	Interval time.Duration
	Renotify time.Duration
	Timeout  time.Duration
}

// BuildOptions carries what the engine package cannot supply itself.
type BuildOptions struct {
	// Command builds the runner for a declared poll source. The engine does not
	// exec: the host owns process policy (which shell, which env, which budget),
	// so a rule file cannot smuggle one in.
	Command func(argv []string) (Runner, error)
}

// SourceFactory builds a source from its spec.
type SourceFactory func(SourceSpec) (Source, error)

var (
	registryMu  sync.RWMutex
	sourceTypes = map[string]SourceFactory{}
	joinFamily  = map[string]func() []AckLevel{}
)

// RegisterSource makes a source type available to rule files. A host-journal
// source registers itself from its own package (see pkg/watch/hostsrc), so the
// engine never imports the stores it reads.
func RegisterSource(typ string, f SourceFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	sourceTypes[typ] = f
}

// RegisterJoin names an ordered ack-level ladder a correlation rule can use.
func RegisterJoin(name string, levels func() []AckLevel) {
	registryMu.Lock()
	defer registryMu.Unlock()
	joinFamily[name] = levels
}

// SourceTypes lists the registered source types.
func SourceTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(sourceTypes))
	for k := range sourceTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build turns a rule file into a plan.
func (f RuleFile) Build(opts BuildOptions) (Plan, error) {
	plan := Plan{Interval: f.Interval.D(), Renotify: f.Renotify.D(), Timeout: f.Timeout.D()}
	for _, spec := range f.Sources {
		src, err := buildSource(spec, opts)
		if err != nil {
			return Plan{}, err
		}
		plan.Sources = append(plan.Sources, src)
	}
	for _, spec := range f.Rules {
		rule, err := buildRule(spec)
		if err != nil {
			return Plan{}, err
		}
		plan.Rules = append(plan.Rules, rule)
	}
	return plan, nil
}

func buildSource(spec SourceSpec, opts BuildOptions) (Source, error) {
	if spec.Type == "poll" {
		if len(spec.Command) == 0 {
			return nil, fmt.Errorf("watch: a poll source needs a command")
		}
		if opts.Command == nil {
			return nil, fmt.Errorf("watch: this host does not run commands from a rule file")
		}
		run, err := opts.Command(spec.Command)
		if err != nil {
			return nil, err
		}
		p := NewPollSource("poll", spec.Locator, run)
		p.Keys, p.AllowEmpty = spec.Keys, spec.AllowEmpty
		return p, nil
	}
	registryMu.RLock()
	factory := sourceTypes[spec.Type]
	registryMu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("watch: unknown source type %q (have: poll, %s)", spec.Type, strings.Join(SourceTypes(), ", "))
	}
	return factory(spec)
}

func buildRule(spec RuleSpec) (Rule, error) {
	switch spec.Type {
	case "match", "":
		return NewMatch(spec.Name, spec.When)
	case "change":
		return NewChange(spec.Name, spec.Fields, spec.When)
	case "inverse", "idle", "absence", "liveness":
		cfg := Inverse{
			RuleName: spec.Name, Within: spec.Within.D(), Reason: spec.Reason,
			Expect: spec.Expect, NoClear: spec.NoClear, Message: spec.Message,
		}
		if spec.Activity != nil {
			pred, err := spec.Activity.Compile()
			if err != nil {
				return nil, err
			}
			cfg.Activity = pred
		}
		// SCOPE DEFAULTS TO ACTIVITY, which is the conservative reading of a
		// rule file that names only one: a rule saying "activity is sprint
		// thread entries" is asking about sprints, and must not start a timer
		// on every unrelated locator another source mentions. A rule file that
		// wants a wider scope than activity — a liveness sample arming a clock
		// only real output resets — says so with "scope".
		scopeSpec := spec.Scope
		if scopeSpec == nil {
			scopeSpec = spec.Activity
		}
		if scopeSpec != nil {
			pred, err := scopeSpec.Compile()
			if err != nil {
				return nil, err
			}
			cfg.Scope = pred
		}
		if spec.LocatorBy != "" {
			fn, err := extractor(spec.LocatorBy)
			if err != nil {
				return nil, err
			}
			cfg.LocatorOf = fn
		}
		if cfg.Reason == "" {
			switch spec.Type {
			case "absence":
				cfg.Reason = ReasonAbsent
			case "liveness":
				cfg.Reason = ReasonMissing
			}
		}
		return NewInverse(cfg), nil
	case "correlation", "reply":
		if spec.Trigger == nil {
			return nil, fmt.Errorf("watch: rule %q: a correlation needs a trigger", spec.Name)
		}
		trigger, err := spec.Trigger.Compile()
		if err != nil {
			return nil, err
		}
		key := spec.Key
		if key == "" {
			key = "attr:conv"
		}
		keyFn, err := extractor(key)
		if err != nil {
			return nil, err
		}
		registryMu.RLock()
		levels := joinFamily[spec.Join]
		registryMu.RUnlock()
		if levels == nil {
			return nil, fmt.Errorf("watch: rule %q: unknown join %q", spec.Name, spec.Join)
		}
		return NewCorrelation(Correlation{
			RuleName: spec.Name, Trigger: trigger, Key: keyFn,
			Levels: levels(), Within: spec.Within.D(),
		}), nil
	default:
		return nil, fmt.Errorf("watch: unknown rule type %q", spec.Type)
	}
}

// extractor reads one string off an event: "locator", "source", "key" or
// "attr:<name>".
func extractor(spec string) (func(Event) string, error) {
	switch {
	case spec == "locator":
		return func(ev Event) string { return ev.Locator }, nil
	case spec == "source":
		return func(ev Event) string { return ev.Source }, nil
	case spec == "key":
		return func(ev Event) string { return ev.Key }, nil
	case strings.HasPrefix(spec, "attr:"):
		name := strings.TrimPrefix(spec, "attr:")
		return func(ev Event) string { return ev.Attr(name) }, nil
	default:
		return nil, fmt.Errorf("watch: %q is not a field selector (want locator, source, key or attr:<name>)", spec)
	}
}

// BuildSource builds one source from its spec, for a caller assembling a plan
// from flags rather than from a file.
func BuildSource(spec SourceSpec, opts BuildOptions) (Source, error) {
	return buildSource(spec, opts)
}
