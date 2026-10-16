package nftcmd

import (
	"bytes"
	"context"
	"encoding/json"
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
