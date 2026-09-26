package resolve

import (
	"reflect"
	"sort"
	"testing"
)

func TestClassifyModel_Tier(t *testing.T) {
	for _, tt := range []struct {
		name    string
		family  string
		params  string
		caps    []string
		wantT   int
		comment string
	}{
		{name: "qwen2.5:0.5b", params: "0.5B", wantT: TierL1, comment: "<3B"},
		{name: "llama3.2:1b", params: "1B", wantT: TierL1},
		{name: "phi-mini:2.7b", params: "2.7B", wantT: TierL1, comment: "edge of L1"},
		{name: "llama3.1:8b", params: "8B", wantT: TierL2, comment: "3-13B"},
		{name: "qwen2.5-coder:7b", params: "7B", wantT: TierL2},
		{name: "qwen2.5:14b", params: "14B", wantT: TierL3, comment: "13-35B"},
		{name: "qwen2.5-coder:32b", params: "32B", wantT: TierL3},
		{name: "llama3.1:70b", params: "70B", wantT: TierL4, comment: ">=35B"},
		{name: "qwen2.5:72b", params: "72B", wantT: TierL4},
		{name: "mixtral:8x7b", params: "8x7B", wantT: TierL2, comment: "MoE — per-expert 7B"},
		{name: "namebut:5b", params: "", wantT: TierL2, comment: "fall back to name regex"},
		{name: "mystery", params: "", wantT: TierL2, comment: "unknown → safe default L2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ClassifyModel(tt.name, tt.family, tt.params, tt.caps)
			if got != tt.wantT {
				t.Errorf("tier(%s, paramSize=%q)=%d, want %d (%s)", tt.name, tt.params, got, tt.wantT, tt.comment)
			}
		})
	}
}

func TestClassifyModel_Domain(t *testing.T) {
	cases := []struct {
		name        string
		family      string
		caps        []string
		wantDomains []string
	}{
		{name: "qwen2.5-coder:7b", wantDomains: []string{DomainCoding}},
		{name: "deepseek-coder:6.7b", wantDomains: []string{DomainCoding}},
		{name: "nomic-embed-text", caps: []string{"embedding"}, wantDomains: []string{DomainEmbedding}},
		{name: "bge-m3", wantDomains: []string{DomainEmbedding}},
		{name: "llava:7b", caps: []string{"vision"}, wantDomains: []string{DomainVision}},
		{name: "llama3.2-vision:11b", wantDomains: []string{DomainVision}},
		{name: "deepseek-r1:7b", wantDomains: []string{DomainReasoning}},
		{name: "marco-o1", wantDomains: []string{DomainReasoning}},
		{name: "llama3.1:8b", wantDomains: []string{DomainGeneral}},
		{name: "mistral:7b", wantDomains: []string{DomainGeneral}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, got := ClassifyModel(tt.name, tt.family, "7B", tt.caps)
			sort.Strings(got)
			want := append([]string{}, tt.wantDomains...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("domains(%s)=%v, want %v", tt.name, got, want)
			}
		})
	}
}

func TestClassifyModel_GeneralDroppedWhenSpecial(t *testing.T) {
	// Coding model should NOT carry the "general" label — substituting a
	// vanilla chat model for a coding-tagged request is the wrong call.
	_, domains := ClassifyModel("qwen2.5-coder:7b", "", "7B", nil)
	for _, d := range domains {
		if d == DomainGeneral {
			t.Errorf("coding model picked up DomainGeneral; domains=%v", domains)
		}
	}
}

func TestClassifyModel_EmptyName(t *testing.T) {
	tier, domains := ClassifyModel("", "", "7B", nil)
	if tier != TierUnknown {
		t.Errorf("tier=%d, want TierUnknown for empty name", tier)
	}
	if len(domains) != 0 {
		t.Errorf("domains=%v, want empty for empty name", domains)
	}
}

func TestClassifyModel_CapabilitiesWinOverName(t *testing.T) {
	// A model that's not obviously embedding by name but reports the
	// "embedding" capability should still classify as embedding.
	_, domains := ClassifyModel("mysterymodel:1b", "", "1B", []string{"embedding"})
	hasEmbed := false
	for _, d := range domains {
		if d == DomainEmbedding {
			hasEmbed = true
		}
	}
	if !hasEmbed {
		t.Errorf("capability-driven embedding tag missing; domains=%v", domains)
	}
}
