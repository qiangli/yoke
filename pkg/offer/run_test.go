package offer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/broker"
)

func TestOfferAgentTokenCapRefusesBeforeLaunch(t *testing.T) {
	rec := broker.OfferRecord{Name: "agent", TokenCap: 40, Definition: json.RawMessage(`{"name":"agent","class":"agent","input":{"properties":{}}}`)}
	_, _, err := runOffer(t.Context(), rec, map[string]any{"token_cap": float64(41)})
	if err == nil || !strings.Contains(err.Error(), "token_cap exceeds offer cap") {
		t.Fatalf("oversize cap: %v", err)
	}
	rec.TokenCap = 0
	_, _, err = runOffer(t.Context(), rec, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no token cap") {
		t.Fatalf("missing cap: %v", err)
	}
	argv, err := agentOfferArgv(ToolDefinition{Name: "agent", Class: "agent"}, 40, map[string]any{"instruction": "task"})
	if err != nil || !strings.Contains(strings.Join(argv, " "), "--max-tokens 40") {
		t.Fatalf("cap not passed to delegate: %q, %v", argv, err)
	}
}

func TestOfferToolArgsTypedNames(t *testing.T) {
	d := ToolDefinition{Input: map[string]any{"properties": map[string]any{"count": map[string]any{"type": "integer"}}}}
	if _, err := toolArgs(d, map[string]any{"other": "x"}); err == nil {
		t.Fatal("unknown argument admitted")
	}
	argv, err := toolArgs(d, map[string]any{"count": float64(3)})
	if err != nil || strings.Join(argv, " ") != "--count 3" {
		t.Fatalf("argv %q: %v", argv, err)
	}
}
