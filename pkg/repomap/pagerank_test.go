package repomap

import (
	"testing"
)

func TestPageRank_Basic(t *testing.T) {
	// Simple graph: A -> B -> C
	graph := map[string][]string{
		"A": {"B"},
		"B": {"C"},
	}

	ranks := pageRank(graph, nil, 20, 0.85)

	// C should have highest rank (most pointed to transitively).
	if ranks["C"] <= ranks["A"] {
		t.Errorf("expected C rank (%f) > A rank (%f)", ranks["C"], ranks["A"])
	}
}

func TestPageRank_Personalization(t *testing.T) {
	graph := map[string][]string{
		"A": {"B", "C"},
		"B": {"C"},
	}

	// Personalize toward A.
	personal := map[string]float64{"A": 10.0}
	ranks := pageRank(graph, personal, 20, 0.85)

	if ranks == nil {
		t.Fatal("expected non-nil ranks")
	}

	// A should have a high rank due to personalization.
	if ranks["A"] <= 0 {
		t.Error("expected positive rank for A")
	}
}

func TestPageRank_EmptyGraph(t *testing.T) {
	ranks := pageRank(nil, nil, 20, 0.85)
	if ranks != nil {
		t.Error("expected nil ranks for empty graph")
	}
}

func TestIsCamelCase(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"HandleRequest", true},
		{"myVar", true},
		{"ALL_CAPS", false},
		{"lowercase", false},
		{"x", false},
	}
	for _, tt := range tests {
		if got := isCamelCase(tt.name); got != tt.want {
			t.Errorf("isCamelCase(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsSnakeCase(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"handle_request", true},
		{"my_var", true},
		{"nounder", false},
		{"has-dash", false},
	}
	for _, tt := range tests {
		if got := isSnakeCase(tt.name); got != tt.want {
			t.Errorf("isSnakeCase(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
