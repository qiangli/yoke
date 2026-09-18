package resources

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

func TestCanonicalProvider(t *testing.T) {
	tests := []struct {
		model, provider, want string
	}{
		{"claude-opus", "anthropic", "Anthropic"},
		{"fable5", "", "Anthropic"},
		{"gpt-5.5", "openai", "OpenAI"},
		{"codex-gpt-5.5", "openai-compat", "OpenAI"},
		{"gemini3.1", "gemini", "Google"},
		{"agy-gemini", "google", "Google"},
		{"glm-5.2", "openai-compat", "Zhipu"},
		{"zhipu-model", "z.ai", "Zhipu"},
		{"kimi-k3", "openai-compat", "Moonshot"},
		{"moonshot-v1", "moonshot", "Moonshot"},
		{"deepseek-v4-pro", "openai-compat", "DeepSeek"},
	}

	for _, tt := range tests {
		got := CanonicalProvider(tt.model, tt.provider)
		if got != tt.want {
			t.Errorf("CanonicalProvider(%q, %q) = %q, want %q", tt.model, tt.provider, got, tt.want)
		}
	}
}

func TestCollectFleetResourcesSchema(t *testing.T) {
	fleettest.Ring(t) // the groups are rolled up from the ring's agents
	ctx := context.Background()
	fr, err := CollectFleetResources(ctx)
	if err != nil {
		t.Fatalf("CollectFleetResources failed: %v", err)
	}

	if fr.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", fr.SchemaVersion, SchemaVersion)
	}

	if len(fr.Groups) == 0 {
		t.Errorf("expected non-empty Groups")
	}

	// Verify busy + idle + cooling + unavailable == total for every group
	for _, g := range fr.Groups {
		if g.Busy+g.Idle+g.Cooling+g.Unavailable != g.Total {
			t.Errorf("Group %s %s sum mismatch: busy(%d) + idle(%d) + cooling(%d) + unavail(%d) != total(%d)",
				g.Provider, g.Band, g.Busy, g.Idle, g.Cooling, g.Unavailable, g.Total)
		}
		if g.Subscription+g.APIKey != g.Total {
			t.Errorf("Group %s %s sub/api mismatch: sub(%d) + api(%d) != total(%d)",
				g.Provider, g.Band, g.Subscription, g.APIKey, g.Total)
		}
	}

	// Verify JSON serialization
	b, err := json.Marshal(fr)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var unmarshaled FleetResources
	if err := json.Unmarshal(b, &unmarshaled); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if unmarshaled.SchemaVersion != SchemaVersion {
		t.Errorf("unmarshaled SchemaVersion = %q, want %q", unmarshaled.SchemaVersion, SchemaVersion)
	}

	// Verify table formatting
	tableStr := FormatTable(fr)
	if !strings.Contains(tableStr, "PROVIDER") || !strings.Contains(tableStr, "BAND") {
		t.Errorf("FormatTable output missing expected header columns:\n%s", tableStr)
	}
}

func TestToolOnlyRunIsAttributedToDeterministicCatalogAgent(t *testing.T) {
	agents := []BoardAgent{
		{Name: "claude-sonnet5", Tool: "claude", Model: "sonnet5", Band: 2, Found: true, Available: true},
		{Name: "claude-opus4.8", Tool: "claude", Model: "opus4.8", Band: 3, Found: true, Available: true},
		{Name: "claude-haiku4.5", Tool: "claude", Model: "haiku4.5", Band: 1, Found: true, Available: true},
		{Name: "claude-fable5", Tool: "claude", Model: "fable5", Band: 4, Found: true, Available: true},
	}
	// This is the #116 record shape: the live identity has only a tool;
	// model and agent are absent (and the separately persisted owner is not an
	// agent identity). The catalog order rule selects claude-fable5.
	runs := []BoardRun{{State: "working", Tool: "claude"}}

	fr, err := CollectFleetResourcesFromBoard(context.Background(), time.Now(), agents, runs)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Totals.Busy != 1 || fr.Totals.Idle != 3 || fr.Totals.Unattributed != 0 {
		t.Fatalf("totals = %+v, want 1 busy, 3 idle, 0 unattributed", fr.Totals)
	}
	for _, group := range fr.Groups {
		if group.Provider == "Anthropic" && group.Band == "L4" {
			if group.Busy != 1 {
				t.Fatalf("Anthropic L4 busy = %d, want 1", group.Busy)
			}
			return
		}
	}
	t.Fatal("Anthropic L4 group not found")
}

func TestUnknownToolRunIsVisibleAsUnattributed(t *testing.T) {
	agents := []BoardAgent{{Name: "claude-fable5", Tool: "claude", Model: "fable5", Band: 4, Found: true, Available: true}}
	runs := []BoardRun{{State: "allocated", Tool: "not-in-the-catalog"}}

	fr, err := CollectFleetResourcesFromBoard(context.Background(), time.Now(), agents, runs)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Totals.Busy != 0 || fr.Totals.Unattributed != 1 {
		t.Fatalf("totals = %+v, want 0 busy and 1 unattributed", fr.Totals)
	}
	if table := FormatTable(fr); !strings.Contains(table, "UNATTRIBUTED") || !strings.Contains(table, "Totals") {
		t.Fatalf("unattributed run is not visible in table:\n%s", table)
	}
}
