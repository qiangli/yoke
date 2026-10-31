//go:build linux

package nft

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRuleJSONFitsKernelUserdata(t *testing.T) {
	// Netavark sends several nested expressions per rule. The kernel rejects
	// NFTA_RULE_USERDATA above NFT_USERDATA_MAXLEN (256) with ERANGE.
	exprs := json.RawMessage(`[{"match":{"left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"10.88.0.0","len":16}},"op":"=="}},{"match":{"left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"224.0.0.0","len":4}},"op":"!="}},{"masquerade":null}]`)
	encoded, err := appendRuleJSON(nil, exprs)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 256 {
		t.Fatalf("rule userdata is %d bytes; kernel limit 256", len(encoded))
	}
	if !bytes.Equal(readRuleJSON(encoded), exprs) {
		t.Fatal("rule JSON did not round trip")
	}
}
