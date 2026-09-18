package sota

import (
	"strings"
	"testing"
	"time"
)

func TestSlugify(t *testing.T) {
	if g := slugify("RFT: Rejection-Sampling Fine-Tuning!"); g != "sota-rft-rejection-sampling-fine-tuning" {
		t.Fatalf("slug = %q", g)
	}
}

func TestBuildPromptGroundedCitesOnlyProvided(t *testing.T) {
	p := buildPrompt("x", nil, false, time.Now())
	if !strings.Contains(p, "cite ONLY these URLs") || !strings.Contains(p, "Today's date") {
		t.Fatalf("grounded prompt missing constraints: %q", p)
	}
}

func TestBuildPromptHitchhikeUsesAgentSearch(t *testing.T) {
	p := buildPrompt("x", nil, true, time.Now())
	if !strings.Contains(p, "USE YOUR WEB SEARCH TOOL") || !strings.Contains(p, "NEVER invent") {
		t.Fatalf("hitchhike prompt missing search instruction: %q", p)
	}
}

func TestVerifyExtractsCitations(t *testing.T) {
	report := "finding one [https://example.com/a] and two [https://example.org/b]."
	urls := dedupURLs(urlRe.FindAllString(report, -1))
	if len(urls) != 2 {
		t.Fatalf("want 2 urls, got %v", urls)
	}
}
