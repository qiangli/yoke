package seedfit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// ConfigSchema is the schema id a --config file must declare.
const ConfigSchema = "bashy-seedfit-config-v1"

// Config selects the matrix capability areas the fit keeps, replacing the
// built-in CapabilityAreas default. Every matrix area must be either listed
// under areas or named in exclude, so no area is ever dropped silently, and
// a listed area with no matrix rows is an error (a typo cannot drop one).
//
//	schema: bashy-seedfit-config-v1
//	areas:
//	  coding-debug-test: {weight: 0.20, hands_on: true}
//	  browser-use:       {weight: 0.15}
//	exclude: [math]
//
// weight and hands_on describe the role profile over the per-area fits
// (--per-area); the single composite fit reads only the area names.
type Config struct {
	Schema  string                `yaml:"schema"`
	Areas   map[string]AreaConfig `yaml:"areas"`
	Exclude []string              `yaml:"exclude"`
	// Anchors are operator-known-good models: the --per-area profile draws
	// its line and hands-on floor from them.
	Anchors []string `yaml:"anchors"`
	// Candidates limits the profile's rows (the fit still uses every row,
	// predecessors included); empty means every fitted model.
	Candidates []string `yaml:"candidates"`
	// Missing sets how a candidate's missing benchmark cell is treated in the
	// per-area fit.
	Missing MissingConfig `yaml:"missing"`
}

// MissingConfig: mode "floor" gives every candidate without a row on a fitted
// benchmark a cell at (lowest observed score − gap), kept above zero, so a
// missing cell always ranks below the worst measured one and is never a real
// zero. Floor cells use the reference harness and source, so no offset can
// absorb them. Empty mode: missing cells are skipped (the fit's default).
//
// min_coverage limits flooring to benchmarks on which at least that fraction
// of the candidates has a row: on a sparse board absence means "not run", not
// "worse than the weakest", so its missing cells are skipped (its data still
// counts). Default 0.5.
type MissingConfig struct {
	Mode        string  `yaml:"mode"`
	Gap         float64 `yaml:"gap"`          // on the 0–1 scale; default 0.05
	MinCoverage float64 `yaml:"min_coverage"` // fraction of candidates; default 0.5
}

// AreaConfig is one included area.
type AreaConfig struct {
	Weight  float64 `yaml:"weight"`
	HandsOn bool    `yaml:"hands_on"`
}

// LoadConfig reads and validates a --config file. Unknown keys are errors.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.Schema != ConfigSchema {
		return nil, fmt.Errorf("config %s: schema %q, want %q", path, c.Schema, ConfigSchema)
	}
	if len(c.Areas) == 0 {
		return nil, fmt.Errorf("config %s: no areas", path)
	}
	excluded := map[string]bool{}
	for _, a := range c.Exclude {
		excluded[a] = true
	}
	switch c.Missing.Mode {
	case "", "floor":
	default:
		return nil, fmt.Errorf("config %s: missing.mode %q, want floor or empty", path, c.Missing.Mode)
	}
	if c.Missing.Gap < 0 || c.Missing.Gap >= 1 {
		return nil, fmt.Errorf("config %s: missing.gap must be in [0,1)", path)
	}
	if c.Missing.MinCoverage < 0 || c.Missing.MinCoverage > 1 {
		return nil, fmt.Errorf("config %s: missing.min_coverage must be in [0,1]", path)
	}
	if c.Missing.Mode == "floor" && c.Missing.Gap == 0 {
		c.Missing.Gap = 0.05
	}
	if c.Missing.Mode == "floor" && c.Missing.MinCoverage == 0 {
		c.Missing.MinCoverage = 0.5
	}
	for name, a := range c.Areas {
		if excluded[name] {
			return nil, fmt.Errorf("config %s: area %q is both included and excluded", path, name)
		}
		if a.Weight < 0 {
			return nil, fmt.Errorf("config %s: area %q has a negative weight", path, name)
		}
	}
	return &c, nil
}

// AreaNames returns the included areas, sorted.
func (c *Config) AreaNames() []string {
	names := make([]string, 0, len(c.Areas))
	for name := range c.Areas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckRows reports every config area with no matrix rows and every matrix
// area the config neither includes nor excludes.
func (c *Config) CheckRows(rows []Row) error {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Area] = true
	}
	known := map[string]bool{}
	var errs []error
	for _, name := range c.AreaNames() {
		known[name] = true
		if !seen[name] {
			errs = append(errs, fmt.Errorf("config area %q has no matrix rows", name))
		}
	}
	for _, name := range c.Exclude {
		known[name] = true
	}
	var unlisted []string
	for a := range seen {
		if !known[a] {
			unlisted = append(unlisted, a)
		}
	}
	sort.Strings(unlisted)
	for _, a := range unlisted {
		errs = append(errs, fmt.Errorf("matrix area %q is neither included nor excluded by the config", a))
	}
	return errors.Join(errs...)
}
