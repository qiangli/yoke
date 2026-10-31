package nftcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/nft"
)

// fake swaps the kernel for pkg/nft's in-memory backend (no root, no netlink).
func fake(t *testing.T) {
	t.Helper()
	b := nft.NewMemBackend()
	old := newBackend
	newBackend = func() (nft.Backend, error) { return b, nil }
	t.Cleanup(func() { newBackend = old })
}

func runNft(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), FS: tool.NewLocalFS(),
		Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader(stdin)}}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestVersionSatisfiesKnftables(t *testing.T) {
	out, _, code := runNft(t, "", "--version")
	// knftables.New rejects "nftables v0." and "nftables v1.0.0 ".
	if code != 0 || !strings.HasPrefix(out, "nftables v1.") || strings.HasPrefix(out, "nftables v1.0.0 ") {
		t.Fatalf("version = %q (%d)", out, code)
	}
}

func TestNetavarkJSONTransaction(t *testing.T) {
	fake(t)
	// Netavark's nftables Batch emits this schema for its inet table,
	// base chains, forwarding jump and subnet masquerade rule.
	tx := `{"nftables":[{"add":{"table":{"family":"inet","name":"netavark"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"FORWARD","type":"filter","hook":"forward","prio":0,"policy":"accept"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"POSTROUTING","type":"nat","hook":"postrouting","prio":100,"policy":"accept"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"nv_123_10_88_0_0_nm16"}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"POSTROUTING","expr":[{"match":{"left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"10.88.0.0","len":16}},"op":"=="}},{"jump":{"target":"nv_123_10_88_0_0_nm16"}}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"nv_123_10_88_0_0_nm16","expr":[{"match":{"left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"224.0.0.0","len":4}},"op":"!="}},{"masquerade":null}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code != 0 {
		t.Fatalf("netavark transaction: %d %s", code, errb)
	}
	out, errb, code := runNft(t, "", "-j", "list", "table", "inet", "netavark")
	if code != 0 || !strings.Contains(out, `"name":"nv_123_10_88_0_0_nm16"`) || !strings.Contains(out, `"chain":"POSTROUTING"`) || !strings.Contains(out, `"masquerade":null`) {
		t.Fatalf("ruleset: %d %s %s", code, out, errb)
	}
}

func TestNetavarkJSONForwardAndPortRules(t *testing.T) {
	fake(t)
	tx := `{"nftables":[{"add":{"table":{"family":"inet","name":"netavark"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"FORWARD","type":"filter","hook":"forward","prio":0,"policy":"accept"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"NETAVARK-HOSTPORT-SETMARK"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"NETAVARK-HOSTPORT-DNAT"}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"FORWARD","expr":[{"match":{"left":{"ct":{"key":"state"}},"right":"invalid","op":"in"}},{"drop":null}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"FORWARD","expr":[{"match":{"left":{"&":[{"ct":{"key":"mark"}},4096]},"right":4096,"op":"=="}},{"accept":null}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"NETAVARK-HOSTPORT-SETMARK","expr":[{"mangle":{"key":{"meta":{"key":"mark"}},"value":{"|":[{"meta":{"key":"mark"}},8192]}}}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"NETAVARK-HOSTPORT-DNAT","expr":[{"match":{"left":{"payload":{"protocol":"tcp","field":"dport"}},"right":8080,"op":"=="}},{"mangle":{"key":{"ct":{"key":"mark"}},"value":{"|":[{"meta":{"key":"mark"}},4096]}}},{"dnat":{"addr":"10.88.0.2","family":"ip","port":80}}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code != 0 {
		t.Fatalf("netavark forward/port transaction: %d %s", code, errb)
	}
	out, errb, code := runNft(t, "", "-j", "list", "table", "inet", "netavark")
	if code != 0 || !strings.Contains(out, `"dnat":{"addr":"10.88.0.2"`) || !strings.Contains(out, `"drop":null`) {
		t.Fatalf("ruleset: %d %s %s", code, out, errb)
	}
	rangeRule := `{"nftables":[{"add":{"rule":{"family":"inet","table":"netavark","chain":"NETAVARK-HOSTPORT-DNAT","expr":[{"match":{"left":{"payload":{"protocol":"tcp","field":"dport"}},"right":{"range":[8080,8085]},"op":"=="}},{"accept":null}]}}}]}`
	if _, errb, code := runNft(t, rangeRule, "-j", "-f", "-"); code != 0 {
		t.Fatalf("port range: %d %s", code, errb)
	}
}

func TestNetavarkJSONSetMapAndTeardown(t *testing.T) {
	fake(t)
	tx := `{"nftables":[{"add":{"table":{"family":"inet","name":"netavark"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"FORWARD"}}},{"add":{"set":{"family":"inet","table":"netavark","name":"allowed","type":"ipv4_addr"}}},{"add":{"map":{"family":"inet","table":"netavark","name":"ports","type":"inet_service","map":"ipv4_addr"}}},{"add":{"element":{"family":"inet","table":"netavark","name":"allowed","elem":["10.88.0.2"]}}},{"add":{"element":{"family":"inet","table":"netavark","name":"ports","elem":[[8080,"10.88.0.2"]]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"FORWARD","expr":[{"accept":null}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code != 0 {
		t.Fatalf("add: %d %s", code, errb)
	}
	out, errb, code := runNft(t, "", "-j", "list", "table", "inet", "netavark")
	if code != 0 || !strings.Contains(out, `"name":"allowed"`) || !strings.Contains(out, `"name":"ports"`) {
		t.Fatalf("list: %d %s %s", code, out, errb)
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var handle uint64
	for _, item := range doc.Nftables {
		if raw := item["rule"]; len(raw) > 0 {
			var r struct{ Handle uint64 }
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			handle = r.Handle
		}
	}
	if handle == 0 {
		t.Fatalf("no rule handle: %s", out)
	}
	del := fmt.Sprintf(`{"nftables":[{"delete":{"rule":{"family":"inet","table":"netavark","chain":"FORWARD","handle":%d,"expr":[{"accept":null}]}}},{"delete":{"table":{"family":"inet","name":"netavark"}}}]}`, handle)
	if _, errb, code := runNft(t, del, "-j", "-f", "-"); code != 0 {
		t.Fatalf("delete: %d %s", code, errb)
	}
	if _, _, code := runNft(t, "", "-j", "list", "table", "inet", "netavark"); code == 0 {
		t.Fatal("table still exists")
	}
}

func TestNetavarkJSONRejectsUnknownStatement(t *testing.T) {
	fake(t)
	tx := `{"nftables":[{"add":{"rule":{"family":"inet","table":"netavark","chain":"FORWARD","expr":[{"queue":null}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code == 0 || !strings.Contains(errb, "unsupported JSON statement") {
		t.Fatalf("unexpected result: %d %s", code, errb)
	}
}

func TestNetavarkJSONInsertRuleFirst(t *testing.T) {
	fake(t)
	tx := `{"nftables":[{"add":{"table":{"family":"inet","name":"netavark"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"INPUT"}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"INPUT","expr":[{"drop":null}]}}},{"insert":{"rule":{"family":"inet","table":"netavark","chain":"INPUT","expr":[{"accept":null}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code != 0 {
		t.Fatalf("insert: %d %s", code, errb)
	}
	out, errb, code := runNft(t, "", "-j", "list", "chain", "inet", "netavark", "INPUT")
	if code != 0 {
		t.Fatalf("list: %d %s", code, errb)
	}
	if strings.Index(out, `"accept":null`) > strings.Index(out, `"drop":null`) {
		t.Fatalf("insert did not prepend: %s", out)
	}
}

func TestNetavarkJSONBridgeSourceShapes(t *testing.T) {
	fake(t)
	tx := `{"nftables":[{"add":{"table":{"family":"inet","name":"netavark"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"INPUT","type":"filter","hook":"input","prio":0,"policy":"accept"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"PREROUTING","type":"nat","hook":"prerouting","prio":-100,"policy":"accept"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"NETAVARK-HOSTPORT-DNAT"}}},{"add":{"chain":{"family":"inet","table":"netavark","name":"nv_123_10_88_0_0_nm16"}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"PREROUTING","expr":[{"match":{"left":{"fib":{"result":"type","flags":["daddr"]}},"right":"local","op":"=="}},{"jump":{"target":"NETAVARK-HOSTPORT-DNAT"}}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"INPUT","expr":[{"match":{"left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"10.88.0.0","len":16}},"op":"=="}},{"match":{"left":{"meta":{"key":"l4proto"}},"right":{"set":["udp","tcp"]},"op":"=="}},{"match":{"left":{"payload":{"protocol":"th","field":"dport"}},"right":53,"op":"=="}},{"accept":null}]}}},{"add":{"rule":{"family":"inet","table":"netavark","chain":"nv_123_10_88_0_0_nm16","expr":[{"match":{"left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"224.0.0.0","len":4}},"op":"!="}},{"snat":{"addr":"192.0.2.10","family":"ip"}}]}}}]}`
	if _, errb, code := runNft(t, tx, "-j", "-f", "-"); code != 0 {
		t.Fatalf("bridge shapes: %d %s", code, errb)
	}
	out, errb, code := runNft(t, "", "-j", "list", "table", "inet", "netavark")
	if code != 0 || !strings.Contains(out, `"fib":{"result":"type"`) || !strings.Contains(out, `"snat":{"addr":"192.0.2.10"`) {
		t.Fatalf("ruleset: %d %s %s", code, out, errb)
	}
}

func TestKnftablesContract(t *testing.T) {
	fake(t)
	// knftables.New's probe, then a transaction, both over stdin.
	if _, errb, code := runNft(t, "add table ip kube-proxy { comment \"test\" ; }\n", "--check", "-f", "-"); code != 0 {
		t.Fatalf("check probe: %d %s", code, errb)
	}
	tx := `add table ip kube-proxy { comment "rules for kube-proxy" ; }
add chain ip kube-proxy services
add map ip kube-proxy service-ips { type ipv4_addr . inet_proto . inet_service : verdict ; comment "ClusterIP, ExternalIP and LoadBalancer IP traffic" ; }
add rule ip kube-proxy services ip daddr . meta l4proto . th dport vmap @service-ips
add element ip kube-proxy service-ips { 10.43.0.10 . udp . 53 : goto services }
`
	if _, errb, code := runNft(t, tx, "-f", "-"); code != 0 {
		t.Fatalf("-f -: %d %s", code, errb)
	}

	out, errb, code := runNft(t, "", "--json", "list", "map", "ip", "kube-proxy", "service-ips")
	if code != 0 {
		t.Fatalf("list map: %d %s", code, errb)
	}
	var v struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Nftables) != 2 {
		t.Fatalf("list map output %q: %v", out, err)
	}
	if !strings.Contains(string(v.Nftables[1]["map"]), `"elem":[[{"concat":["10.43.0.10","udp",53]},{"goto":{"target":"services"}}]]`) {
		t.Fatalf("map = %s", v.Nftables[1]["map"])
	}

	if out, _, code = runNft(t, "", "--json", "--terse", "list", "table", "ip", "kube-proxy"); code != 0 || !strings.Contains(out, `"chain":{`) || strings.Contains(out, `"elem"`) {
		t.Fatalf("terse list table: %d %q", code, out)
	}
	if out, _, code = runNft(t, "", "-j", "list", "counters", "table", "ip", "kube-proxy"); code != 0 || !strings.Contains(out, `"metainfo"`) {
		t.Fatalf("list counters: %d %q", code, out)
	}

	// Not-found: non-zero exit, and the phrase knftables.IsNotFound looks
	// for on the FIRST stderr line.
	_, errb, code = runNft(t, "", "--json", "list", "set", "ip", "kube-proxy", "absent")
	if code != 1 || !strings.Contains(strings.SplitN(errb, "\n", 2)[0], "No such file or directory") {
		t.Fatalf("absent set: %d %q", code, errb)
	}
	_, errb, code = runNft(t, "delete table ip nope\n", "-f", "-")
	if code != 1 || !strings.Contains(strings.SplitN(errb, "\n", 2)[0], "No such file or directory") {
		t.Fatalf("delete absent table: %d %q", code, errb)
	}

	// Operand form, same language.
	if _, errb, code = runNft(t, "", "add", "chain", "ip", "kube-proxy", "extra"); code != 0 {
		t.Fatalf("operand form: %d %s", code, errb)
	}
}

func TestUnsupportedFailsLoudly(t *testing.T) {
	fake(t)
	for _, args := range [][]string{
		{"list", "ruleset"},       // text listing
		{"-j", "list", "ruleset"}, // outside the subset
		{"--handle", "-f", "-"},   // unknown flag
		{"-f", "-", "extra"},      // stray operand
		{},                        // no command
	} {
		if _, errb, code := runNft(t, "", args...); code == 0 || errb == "" {
			t.Errorf("%q: want failure with a diagnostic, got %d", args, code)
		}
	}
	if _, errb, code := runNft(t, "add rule ip t c log\n", "-f", "-"); code != 1 || !strings.HasPrefix(errb, "Error: ") {
		t.Errorf("unsupported statement: %d %q", code, errb)
	}
}
