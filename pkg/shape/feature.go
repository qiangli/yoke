// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package shape

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const FeatureSchemaV1 = "bashy-shape-feature-v1"

// SchemaRange is the inclusive app-data schema interval a feature can read.
// Zero for Max means no upper bound; an all-zero range is unconstrained.
type SchemaRange struct {
	Min int `json:"min,omitempty" yaml:"min,omitempty"`
	Max int `json:"max,omitempty" yaml:"max,omitempty"`
}

// Feature is the versioned shape.yaml contract.
type Feature struct {
	SchemaVersion string      `json:"schema_version" yaml:"schema_version"`
	ID            string      `json:"id,omitempty" yaml:"id,omitempty"`
	Origin        string      `json:"origin" yaml:"origin"`
	BaseApp       string      `json:"base_app" yaml:"base_app"`
	Revision      string      `json:"revision" yaml:"revision"`
	Requires      []string    `json:"requires,omitempty" yaml:"requires,omitempty"`
	Conflicts     []string    `json:"conflicts,omitempty" yaml:"conflicts,omitempty"`
	Scope         []string    `json:"scope,omitempty" yaml:"scope,omitempty"`
	Schema        SchemaRange `json:"schema_range,omitempty" yaml:"schema_range,omitempty"`
}

// FeatureMetadata is a compatibility alias for callers that prefer the full
// contract name.
type FeatureMetadata = Feature

// ReadFeature reads and validates one versioned shape.yaml document.
func ReadFeature(path string) (Feature, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Feature{}, fmt.Errorf("shape: read feature metadata: %w", err)
	}
	return ParseFeature(b)
}

// ReadMetadata is an alias for ReadFeature.
func ReadMetadata(path string) (Feature, error) { return ReadFeature(path) }

// ParseFeature decodes one shape.yaml document. Unknown fields are rejected so
// a misspelled security or compatibility constraint cannot silently disappear.
func ParseFeature(b []byte) (Feature, error) {
	var f Feature
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return Feature{}, fmt.Errorf("shape: decode feature metadata: %w", err)
	}
	if err := f.Validate(); err != nil {
		return Feature{}, err
	}
	return f, nil
}

// Validate checks one metadata record independently of a selection.
func (f Feature) Validate() error {
	if f.SchemaVersion != FeatureSchemaV1 {
		return fmt.Errorf("shape: unsupported feature schema %q", f.SchemaVersion)
	}
	if strings.TrimSpace(f.Origin) == "" || strings.TrimSpace(f.BaseApp) == "" || strings.TrimSpace(f.Revision) == "" {
		return errors.New("shape: feature metadata needs origin, base_app, and revision")
	}
	if f.Schema.Min < 0 || f.Schema.Max < 0 || (f.Schema.Max != 0 && f.Schema.Min > f.Schema.Max) {
		return fmt.Errorf("shape: invalid schema range %d..%d", f.Schema.Min, f.Schema.Max)
	}
	return nil
}

// CheckCompatible detects combinations that cannot be activated together.
func CheckCompatible(features []Feature) error {
	if len(features) == 0 {
		return nil
	}
	base := ""
	keys := map[string]int{}
	minSchema, maxSchema := 0, 0
	for i, f := range features {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("shape: feature %d: %w", i, err)
		}
		if base == "" {
			base = f.BaseApp
		} else if f.BaseApp != base {
			return fmt.Errorf("shape: incompatible base apps %q and %q", base, f.BaseApp)
		}
		for _, key := range []string{f.ID, f.Revision} {
			if key == "" {
				continue
			}
			if owner, exists := keys[key]; exists && owner != i {
				return fmt.Errorf("shape: duplicate feature identity %q", key)
			}
			keys[key] = i
		}
		if f.Schema.Min > minSchema {
			minSchema = f.Schema.Min
		}
		if f.Schema.Max != 0 && (maxSchema == 0 || f.Schema.Max < maxSchema) {
			maxSchema = f.Schema.Max
		}
	}
	if maxSchema != 0 && minSchema > maxSchema {
		return fmt.Errorf("shape: incompatible schema ranges have no common version")
	}
	for _, f := range features {
		for _, req := range f.Requires {
			if _, ok := keys[req]; !ok {
				return fmt.Errorf("shape: feature %q requires %q", featureName(f), req)
			}
		}
		for _, conflict := range f.Conflicts {
			if _, ok := keys[conflict]; ok {
				return fmt.Errorf("shape: feature %q conflicts with %q", featureName(f), conflict)
			}
		}
	}
	return nil
}

// Compatible reports the same decision as CheckCompatible without discarding
// its explanation.
func Compatible(features []Feature) (bool, error) {
	err := CheckCompatible(features)
	return err == nil, err
}

func featureName(f Feature) string {
	if f.ID != "" {
		return f.ID
	}
	return f.Revision
}
