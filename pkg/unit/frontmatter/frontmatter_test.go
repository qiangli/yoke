// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package frontmatter

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	block, rest, err := Split([]byte("---\nname: x\n---\nbody\n"))
	if err != nil || block != "name: x" || rest != "---\nbody\n" {
		t.Errorf("Split = %q %q %v", block, rest, err)
	}
	// CRLF opening fence; the closing cut is byte-for-byte as the skills
	// writer has always made it.
	block, rest, err = Split([]byte("---\r\nname: x\r\n---\r\nbody"))
	if err != nil || block != "name: x\r" || rest != "---\r\nbody" {
		t.Errorf("Split CRLF = %q %q %v", block, rest, err)
	}
	if _, _, err := Split([]byte("no block\n")); err == nil || !strings.Contains(err.Error(), "no --- block") {
		t.Errorf("missing block err = %v", err)
	}
	if _, _, err := Split([]byte("---\nname: x\n")); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("unterminated err = %v", err)
	}
}

func TestRead(t *testing.T) {
	b := Read("name: uv\n" +
		"description: >\n  Build and test uv.\n  Use when asked to build it.\n" +
		"type: dag # OKF page type\n" +
		"license: MIT-0\n" +
		"metadata:\n  default: fmt-check\n  Tier: local\n")
	if b.YAMLErr != "" {
		t.Fatalf("YAMLErr = %q", b.YAMLErr)
	}
	if b.Name != "uv" || b.Type != "dag" {
		t.Errorf("name/type = %q/%q", b.Name, b.Type)
	}
	if b.Description != "Build and test uv. Use when asked to build it." {
		t.Errorf("description = %q", b.Description)
	}
	// Unknown keys are kept, lower-cased for lookup.
	if lic := b.Top["license"]; Scalar(lic) != "MIT-0" {
		t.Errorf("license = %v", lic)
	}
	// Metadata keeps the author's key case; lookups are lower-cased.
	if !reflect.DeepEqual(b.Metadata, map[string]string{"default": "fmt-check", "Tier": "local"}) {
		t.Errorf("Metadata = %v", b.Metadata)
	}
	if Scalar(b.Meta["tier"]) != "local" {
		t.Errorf("meta tier = %v", b.Meta["tier"])
	}
	// Keys are matched case-insensitively, like both readings always did.
	if Read("Name: X\n").Name != "X" {
		t.Error("capitalized key not read")
	}
}

func TestReadRecordsWhyNotYAML(t *testing.T) {
	if b := Read("description: runs it: fast\n"); b.YAMLErr == "" {
		t.Errorf("mapping-value error not recorded: %+v", b)
	}
	if b := Read("- a\n- b\n"); b.YAMLErr != "frontmatter is not a key: value mapping" {
		t.Errorf("non-mapping YAMLErr = %q", b.YAMLErr)
	}
	if b := Read(""); b.YAMLErr != "" || b.Top != nil {
		t.Errorf("empty block = %+v", b)
	}
	if b := Read("metadata: [1]\n"); b.YAMLErr != "" || b.Metadata != nil || b.Meta != nil {
		t.Errorf("metadata not a mapping = %+v", b)
	}
}

func TestParseEndToEnd(t *testing.T) {
	b, err := Parse([]byte("---\nname: x\n---\n"))
	if err != nil || b.Name != "x" {
		t.Errorf("Parse = %+v %v", b, err)
	}
	if _, err := Parse([]byte("nothing\n")); err == nil {
		t.Error("no-block document accepted")
	}
}

func TestLineKV(t *testing.T) {
	cases := []struct {
		line   string
		key    string
		value  string
		wantOK bool
	}{
		{"name: uv", "name", "uv", true},
		{"Description: Build it ", "description", "Build it", true},
		{"default: \"test\"", "default", "test", true},
		{"quoted: 'a: b'", "quoted", "a: b", true},
		{"nocolon", "", "", false},
		{": lead", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := LineKV(c.line)
		if ok != c.wantOK || k != c.key || v != c.value {
			t.Errorf("LineKV(%q) = %q %q %v", c.line, k, v, ok)
		}
	}
}
