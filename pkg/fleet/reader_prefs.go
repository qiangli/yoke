package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvBashyPrefs is the environment variable projected by launchers carrying
// session reader preferences as JSON.
const EnvBashyPrefs = "BASHY_PREFS"

// View options for output shape.
const (
	ViewText      = "text"
	ViewJSON      = "json"
	ViewYAML      = "yaml"
	ViewMD        = "md"
	ViewCanonical = "canonical"
)

// Help options for instruction delivery form.
const (
	HelpClassic = "classic"
	HelpScript  = "script"
	HelpSkill   = "skill"
	HelpMCP     = "mcp"
)

// Dialect options for command execution.
const (
	DialectPWSH = "pwsh"
	DialectCMD  = "cmd"
)

// DefaultBudgetBytes is the inherited conversational ceiling (40 KiB).
const DefaultBudgetBytes int64 = 40 * 1024

// ReaderPrefs declares preferences for how an agent consumes command outputs
// and help, and what per-command default options are applied.
type ReaderPrefs struct {
	View        string                    `yaml:"view,omitempty" json:"view,omitempty" doc:"output shape (json, yaml, md, text)"`
	Help        string                    `yaml:"help,omitempty" json:"help,omitempty" doc:"help/instruction form (skill, script, mcp, classic)"`
	Defaults    map[string]map[string]any `yaml:"defaults,omitempty" json:"defaults,omitempty" doc:"glob defaults per command like PSDefaultParameterValues"`
	Dialect     string                    `yaml:"dialect,omitempty" json:"dialect,omitempty" doc:"command dialect (bash, pwsh, cmd)"`
	BudgetBytes int64                     `yaml:"budget_bytes,omitempty" json:"budget_bytes,omitempty" doc:"output-reduction budget in bytes"`
	Extra       map[string]any            `yaml:",inline" json:"-" doc:"additional underscore keys"`
}

// GlobDefaults returns Defaults (alias for command glob defaults).
func (p ReaderPrefs) GlobDefaults() map[string]map[string]any {
	return p.Defaults
}

// IsEmpty reports whether the preference set has no declared fields.
func (p ReaderPrefs) IsEmpty() bool {
	return p.View == "" &&
		p.Help == "" &&
		len(p.Defaults) == 0 &&
		p.Dialect == "" &&
		p.BudgetBytes == 0 &&
		len(p.Extra) == 0
}

// Clone returns a deep copy of ReaderPrefs.
func (p ReaderPrefs) Clone() ReaderPrefs {
	res := p
	if p.Defaults != nil {
		res.Defaults = make(map[string]map[string]any, len(p.Defaults))
		for pat, opts := range p.Defaults {
			if opts != nil {
				clonedOpts := make(map[string]any, len(opts))
				for k, v := range opts {
					clonedOpts[k] = v
				}
				res.Defaults[pat] = clonedOpts
			} else {
				res.Defaults[pat] = nil
			}
		}
	}
	if p.Extra != nil {
		res.Extra = make(map[string]any, len(p.Extra))
		for k, v := range p.Extra {
			res.Extra[k] = v
		}
	}
	return res
}

// Merge returns a new ReaderPrefs where non-empty fields in p take precedence over lower.
// Empty values in p inherit from lower. Underscore keys are preserved.
func (p ReaderPrefs) Merge(lower ReaderPrefs) ReaderPrefs {
	res := lower.Clone()

	if p.View != "" {
		res.View = p.View
	}
	if p.Help != "" {
		res.Help = p.Help
	}
	if p.Dialect != "" {
		res.Dialect = p.Dialect
	}
	if p.BudgetBytes > 0 {
		res.BudgetBytes = p.BudgetBytes
	}

	if len(p.Defaults) > 0 {
		if res.Defaults == nil {
			res.Defaults = make(map[string]map[string]any)
		}
		for pattern, opts := range p.Defaults {
			existing, ok := res.Defaults[pattern]
			if !ok || existing == nil {
				mergedOpts := make(map[string]any, len(opts))
				for k, v := range opts {
					mergedOpts[k] = v
				}
				res.Defaults[pattern] = mergedOpts
			} else {
				mergedOpts := make(map[string]any, len(existing)+len(opts))
				for k, v := range existing {
					mergedOpts[k] = v
				}
				for k, v := range opts {
					mergedOpts[k] = v
				}
				res.Defaults[pattern] = mergedOpts
			}
		}
	}

	if len(p.Extra) > 0 {
		if res.Extra == nil {
			res.Extra = make(map[string]any, len(p.Extra))
		}
		for k, v := range p.Extra {
			res.Extra[k] = v
		}
	}

	return res
}

// UnmarshalJSON unmarshals JSON into ReaderPrefs, preserving underscore keys
// and accepting either defaults or glob_defaults.
func (p *ReaderPrefs) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if p.Extra == nil {
		p.Extra = make(map[string]any)
	}
	for k, v := range raw {
		switch k {
		case "view":
			if err := json.Unmarshal(v, &p.View); err != nil {
				return err
			}
		case "help":
			if err := json.Unmarshal(v, &p.Help); err != nil {
				return err
			}
		case "dialect":
			if err := json.Unmarshal(v, &p.Dialect); err != nil {
				return err
			}
		case "budget_bytes":
			if err := json.Unmarshal(v, &p.BudgetBytes); err != nil {
				return err
			}
		case "defaults":
			if err := json.Unmarshal(v, &p.Defaults); err != nil {
				return err
			}
		case "glob_defaults":
			var gd map[string]map[string]any
			if err := json.Unmarshal(v, &gd); err != nil {
				return err
			}
			if p.Defaults == nil {
				p.Defaults = gd
			}
		default:
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return err
			}
			p.Extra[k] = val
		}
	}
	return nil
}

// MarshalJSON marshals ReaderPrefs to JSON, including Extra underscore keys.
func (p ReaderPrefs) MarshalJSON() ([]byte, error) {
	m := make(map[string]any)
	for k, v := range p.Extra {
		m[k] = v
	}
	if p.View != "" {
		m["view"] = p.View
	}
	if p.Help != "" {
		m["help"] = p.Help
	}
	if len(p.Defaults) > 0 {
		m["defaults"] = p.Defaults
	}
	if p.Dialect != "" {
		m["dialect"] = p.Dialect
	}
	if p.BudgetBytes > 0 {
		m["budget_bytes"] = p.BudgetBytes
	}
	return json.Marshal(m)
}

// UnmarshalYAML unmarshals YAML into ReaderPrefs, supporting both defaults
// and glob_defaults.
func (p *ReaderPrefs) UnmarshalYAML(value *yaml.Node) error {
	type rawPrefs ReaderPrefs
	var raw rawPrefs
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*p = ReaderPrefs(raw)
	if p.Defaults == nil {
		var extra struct {
			GlobDefaults map[string]map[string]any `yaml:"glob_defaults"`
		}
		if err := value.Decode(&extra); err == nil && extra.GlobDefaults != nil {
			p.Defaults = extra.GlobDefaults
		}
	}
	return nil
}

// BashyDefaultPrefs returns the baseline bashy defaults.
func BashyDefaultPrefs() ReaderPrefs {
	return ReaderPrefs{
		View:        ViewText,
		Help:        HelpClassic,
		Dialect:     DialectBash,
		BudgetBytes: DefaultBudgetBytes,
	}
}

// BandDefaultPrefs returns the capability band defaults.
// Low-band models (L1-L2) get scripts; premium models (L3+) get skills.
func BandDefaultPrefs(band int) ReaderPrefs {
	if band <= 0 {
		return ReaderPrefs{}
	}
	if band <= 2 {
		return ReaderPrefs{
			Help: HelpScript,
		}
	}
	return ReaderPrefs{
		Help: HelpSkill,
	}
}

// ParseReaderPrefsJSON parses a JSON string into ReaderPrefs.
// An empty or whitespace-only string returns an empty ReaderPrefs and nil error.
func ParseReaderPrefsJSON(s string) (ReaderPrefs, error) {
	if strings.TrimSpace(s) == "" {
		return ReaderPrefs{}, nil
	}
	var p ReaderPrefs
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ReaderPrefs{}, fmt.Errorf("fleet: invalid %s: %w", EnvBashyPrefs, err)
	}
	return p, nil
}

// Source names indicating where an effective preference value originated.
const (
	SourceExplicit = "explicit"
	SourceEnv      = "env"
	SourceAgent    = "agent"
	SourceTool     = "tool"
	SourceModel    = "model"
	SourceBand     = "band"
	SourceDefault  = "default"

	// Descriptive aliases matching session and capability nomenclature.
	SourceBashyPrefs   = SourceEnv
	SourceModelBand    = SourceBand
	SourceBashyDefault = SourceDefault
)

// ReaderPrefsSources records the source layer of each effective preference field.
type ReaderPrefsSources struct {
	View        string                       `yaml:"view,omitempty" json:"view,omitempty" doc:"source of effective view"`
	Help        string                       `yaml:"help,omitempty" json:"help,omitempty" doc:"source of effective help"`
	Dialect     string                       `yaml:"dialect,omitempty" json:"dialect,omitempty" doc:"source of effective dialect"`
	BudgetBytes string                       `yaml:"budget_bytes,omitempty" json:"budget_bytes,omitempty" doc:"source of effective budget_bytes"`
	Defaults    map[string]map[string]string `yaml:"defaults,omitempty" json:"defaults,omitempty" doc:"source of each effective default option per pattern"`
	Extra       map[string]string            `yaml:",inline" json:"-" doc:"source of additional underscore keys"`
}

// Clone returns a deep copy of ReaderPrefsSources.
func (s ReaderPrefsSources) Clone() ReaderPrefsSources {
	res := s
	if s.Defaults != nil {
		res.Defaults = make(map[string]map[string]string, len(s.Defaults))
		for pat, opts := range s.Defaults {
			if opts != nil {
				clonedOpts := make(map[string]string, len(opts))
				for k, v := range opts {
					clonedOpts[k] = v
				}
				res.Defaults[pat] = clonedOpts
			} else {
				res.Defaults[pat] = nil
			}
		}
	}
	if s.Extra != nil {
		res.Extra = make(map[string]string, len(s.Extra))
		for k, v := range s.Extra {
			res.Extra[k] = v
		}
	}
	return res
}

// SourceForDefault returns the source of the given command glob pattern and option key,
// or empty string if not declared.
func (s ReaderPrefsSources) SourceForDefault(pattern, key string) string {
	if s.Defaults == nil {
		return ""
	}
	opts, ok := s.Defaults[pattern]
	if !ok || opts == nil {
		return ""
	}
	return opts[key]
}

// SourceFor returns the source of a field by name ("view", "help", "dialect", "budget_bytes",
// or an underscore extra key).
func (s ReaderPrefsSources) SourceFor(field string) string {
	switch field {
	case "view":
		return s.View
	case "help":
		return s.Help
	case "dialect":
		return s.Dialect
	case "budget_bytes":
		return s.BudgetBytes
	default:
		if s.Extra != nil {
			return s.Extra[field]
		}
		return ""
	}
}

// Source is an alias for SourceFor.
func (s ReaderPrefsSources) Source(field string) string {
	return s.SourceFor(field)
}

// MarshalJSON marshals ReaderPrefsSources to JSON, including Extra keys.
func (s ReaderPrefsSources) MarshalJSON() ([]byte, error) {
	m := make(map[string]any)
	for k, v := range s.Extra {
		m[k] = v
	}
	if s.View != "" {
		m["view"] = s.View
	}
	if s.Help != "" {
		m["help"] = s.Help
	}
	if len(s.Defaults) > 0 {
		m["defaults"] = s.Defaults
	}
	if s.Dialect != "" {
		m["dialect"] = s.Dialect
	}
	if s.BudgetBytes != "" {
		m["budget_bytes"] = s.BudgetBytes
	}
	return json.Marshal(m)
}

// UnmarshalJSON unmarshals JSON into ReaderPrefsSources, preserving underscore extra keys.
func (s *ReaderPrefsSources) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if s.Extra == nil {
		s.Extra = make(map[string]string)
	}
	for k, v := range raw {
		switch k {
		case "view":
			if err := json.Unmarshal(v, &s.View); err != nil {
				return err
			}
		case "help":
			if err := json.Unmarshal(v, &s.Help); err != nil {
				return err
			}
		case "dialect":
			if err := json.Unmarshal(v, &s.Dialect); err != nil {
				return err
			}
		case "budget_bytes":
			if err := json.Unmarshal(v, &s.BudgetBytes); err != nil {
				return err
			}
		case "defaults":
			if err := json.Unmarshal(v, &s.Defaults); err != nil {
				return err
			}
		default:
			var val string
			if err := json.Unmarshal(v, &val); err == nil {
				s.Extra[k] = val
			}
		}
	}
	return nil
}

// ReaderPrefsOptions holds the configuration for resolving reader preferences.
type ReaderPrefsOptions struct {
	Explicit ReaderPrefs
	EnvJSON  string // raw BASHY_PREFS JSON string
	Agent    *Agent
	Tool     *Tool
	Model    *Model
	Band     int
}

// ResolveReaderPrefsWithSources resolves effective ReaderPrefs and traces the source
// of each effective declared preference field using precedence:
// explicit flag > BASHY_PREFS JSON env > agent > tool > model band > bashy default.
func ResolveReaderPrefsWithSources(opts ReaderPrefsOptions) (ReaderPrefs, ReaderPrefsSources, error) {
	// Base layer: bashy default
	res := BashyDefaultPrefs()
	src := ReaderPrefsSources{
		View:        SourceDefault,
		Help:        SourceDefault,
		Dialect:     SourceDefault,
		BudgetBytes: SourceDefault,
		Defaults:    make(map[string]map[string]string),
		Extra:       make(map[string]string),
	}

	applyLayer := func(p ReaderPrefs, sourceName string) {
		res = p.Merge(res)

		if p.View != "" {
			src.View = sourceName
		}
		if p.Help != "" {
			src.Help = sourceName
		}
		if p.Dialect != "" {
			src.Dialect = sourceName
		}
		if p.BudgetBytes > 0 {
			src.BudgetBytes = sourceName
		}
		if len(p.Defaults) > 0 {
			if src.Defaults == nil {
				src.Defaults = make(map[string]map[string]string)
			}
			for pattern, pOpts := range p.Defaults {
				if len(pOpts) == 0 {
					continue
				}
				if src.Defaults[pattern] == nil {
					src.Defaults[pattern] = make(map[string]string)
				}
				for k := range pOpts {
					src.Defaults[pattern][k] = sourceName
				}
			}
		}
		if len(p.Extra) > 0 {
			if src.Extra == nil {
				src.Extra = make(map[string]string)
			}
			for k := range p.Extra {
				src.Extra[k] = sourceName
			}
		}
	}

	// Band layer:
	band := opts.Band
	if band == 0 && opts.Agent != nil && opts.Agent.Band > 0 {
		band = opts.Agent.Band
	}
	if band == 0 && opts.Model != nil && opts.Model.Band > 0 {
		band = opts.Model.Band
	}
	if band > 0 {
		applyLayer(BandDefaultPrefs(band), SourceBand)
	}
	if opts.Model != nil && !opts.Model.Prefs.IsEmpty() {
		applyLayer(opts.Model.Prefs, SourceModel)
	}

	// Tool layer:
	if opts.Tool != nil && !opts.Tool.Prefs.IsEmpty() {
		applyLayer(opts.Tool.Prefs, SourceTool)
	}

	// Agent layer:
	if opts.Agent != nil && !opts.Agent.Prefs.IsEmpty() {
		applyLayer(opts.Agent.Prefs, SourceAgent)
	}

	// Env layer (BASHY_PREFS JSON):
	if strings.TrimSpace(opts.EnvJSON) != "" {
		envPrefs, err := ParseReaderPrefsJSON(opts.EnvJSON)
		if err != nil {
			return ReaderPrefs{}, ReaderPrefsSources{}, err
		}
		applyLayer(envPrefs, SourceEnv)
	}

	// Explicit flag layer:
	if !opts.Explicit.IsEmpty() {
		applyLayer(opts.Explicit, SourceExplicit)
	}

	return res, src, nil
}

// ResolveReaderPrefsWithOptions resolves the effective ReaderPrefs using the precedence order:
// explicit flag > BASHY_PREFS JSON env > agent > tool > band > bashy default.
func ResolveReaderPrefsWithOptions(opts ReaderPrefsOptions) (ReaderPrefs, error) {
	prefs, _, err := ResolveReaderPrefsWithSources(opts)
	return prefs, err
}

// ResolveReaderPrefs is the pure resolver function with precedence:
// explicit flag > BASHY_PREFS JSON env > agent > tool > band > bashy default.
func ResolveReaderPrefs(explicit ReaderPrefs, envJSON string, agent *Agent, tool *Tool, band int) (ReaderPrefs, error) {
	return ResolveReaderPrefsWithOptions(ReaderPrefsOptions{
		Explicit: explicit,
		EnvJSON:  envJSON,
		Agent:    agent,
		Tool:     tool,
		Band:     band,
	})
}

// ResolveReaderPrefsFromEnv resolves reader preferences, reading BASHY_PREFS from os.Getenv.
func ResolveReaderPrefsFromEnv(explicit ReaderPrefs, agent *Agent, tool *Tool, band int) (ReaderPrefs, error) {
	return ResolveReaderPrefs(explicit, os.Getenv(EnvBashyPrefs), agent, tool, band)
}

// ResolveReaderPrefsWithSourcesFromArgs resolves reader preferences with sources from positional arguments.
func ResolveReaderPrefsWithSourcesFromArgs(explicit ReaderPrefs, envJSON string, agent *Agent, tool *Tool, band int) (ReaderPrefs, ReaderPrefsSources, error) {
	return ResolveReaderPrefsWithSources(ReaderPrefsOptions{
		Explicit: explicit,
		EnvJSON:  envJSON,
		Agent:    agent,
		Tool:     tool,
		Band:     band,
	})
}

// ResolveReaderPrefsWithSourcesFromEnv resolves reader preferences with sources, reading BASHY_PREFS from os.Getenv.
func ResolveReaderPrefsWithSourcesFromEnv(explicit ReaderPrefs, agent *Agent, tool *Tool, band int) (ReaderPrefs, ReaderPrefsSources, error) {
	return ResolveReaderPrefsWithSources(ReaderPrefsOptions{
		Explicit: explicit,
		EnvJSON:  os.Getenv(EnvBashyPrefs),
		Agent:    agent,
		Tool:     tool,
		Band:     band,
	})
}
