// Behavioral provenance: argv and payloads are the ones flannel v0.28.4
// (pkg/trafficmngr/iptables, as embedded by k3s v1.37.1+k3s1) builds and
// go-iptables v0.8.0 execs; exit-status meanings (1 = "doesn't exist" /
// "already exists", 2 = bad usage) follow the iptables(8) manual.
package iptablescmd

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/nft"
)

func fake(t *testing.T) *nft.MemBackend {
	t.Helper()
	b := nft.NewMemBackend()
	old := newBackend
	newBackend = func() (nft.Backend, error) { return b, nil }
	t.Cleanup(func() { newBackend = old })
	return b
}

func run(t *testing.T, tl *tool.Tool, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader(stdin)}}
	code := tl.Run(rc, args)
	return out.String(), errb.String(), code
}

func ipt(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, _, code := run(t, iptablesCmd, "", append(args, "--wait")...) // go-iptables appends --wait
	return out, code
}

const masq = "flanneld masq"

// flannel's masq rules for cluster 10.42.0.0/16, lease 10.42.1.0/24.
var masqRules = [][]string{
	{"POSTROUTING", "-m", "comment", "--comment", masq, "-j", "FLANNEL-POSTRTG"},
	{"FLANNEL-POSTRTG", "-m", "mark", "--mark", "0x4000/0x4000", "-m", "comment", "--comment", masq, "-j", "RETURN"},
	{"FLANNEL-POSTRTG", "-s", "10.42.1.0/24", "-d", "10.42.0.0/16", "-m", "comment", "--comment", masq, "-j", "RETURN"},
	{"FLANNEL-POSTRTG", "-s", "10.42.0.0/16", "-d", "10.42.1.0/24", "-m", "comment", "--comment", masq, "-j", "RETURN"},
	{"FLANNEL-POSTRTG", "!", "-s", "10.42.0.0/16", "-d", "10.42.1.0/24", "-m", "comment", "--comment", masq, "-j", "RETURN"},
	{"FLANNEL-POSTRTG", "-s", "10.42.0.0/16", "!", "-d", "224.0.0.0/4", "-m", "comment", "--comment", masq, "-j", "MASQUERADE", "--random-fully"},
	{"FLANNEL-POSTRTG", "!", "-s", "10.42.0.0/16", "-d", "10.42.0.0/16", "-m", "comment", "--comment", masq, "-j", "MASQUERADE", "--random-fully"},
}

// payload builds what flannel's buildIPTablesRestorePayload writes.
func payload(table string, lines [][]string) string {
	var b strings.Builder
	b.WriteString("*" + table + "\n")
	for _, l := range lines {
		for i, w := range l {
			if i > 0 {
				b.WriteByte(' ')
			}
			if i > 0 && l[i-1] == "--comment" {
				w = `"` + w + `"`
			}
			b.WriteString(w)
		}
		b.WriteByte('\n')
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

func withAction(action string, rules [][]string) [][]string {
	var out [][]string
	for _, r := range rules {
		out = append(out, append([]string{action}, r...))
	}
	return out
}

func TestGoIptablesVersionProbe(t *testing.T) {
	// go-iptables: `v([0-9]+)\.([0-9]+)\.([0-9]+)(?:\s+\((\w+))?`; >= 1.6.2
	// enables --wait and --random-fully.
	re := regexp.MustCompile(`^(ip6?tables|ip6?tables-restore) v1\.8\.10 \(nf_tables\)\n$`)
	for _, tl := range []*tool.Tool{iptablesCmd, ip6tablesCmd, restoreCmd, restore6Cmd} {
		if out, _, code := run(t, tl, "", "--version"); code != 0 || !re.MatchString(out) {
			t.Errorf("%s --version = %q (%d)", tl.Name, out, code)
		}
	}
}

func TestFlannelLifecycle(t *testing.T) {
	b := fake(t)

	// ClearChain: -N, and on "already exists" (exit 1) -F.
	if _, code := ipt(t, "-t", "nat", "-N", "FLANNEL-POSTRTG"); code != 0 {
		t.Fatalf("-N: %d", code)
	}
	if _, code := ipt(t, "-t", "nat", "-N", "FLANNEL-POSTRTG"); code != 1 {
		t.Fatalf("-N existing: want exit 1, got %d", code)
	}
	if _, code := ipt(t, "-t", "nat", "-F", "FLANNEL-POSTRTG"); code != 0 {
		t.Fatalf("-F: %d", code)
	}
	// ChainExists: -S CHAIN 1.
	if _, code := ipt(t, "-t", "nat", "-S", "FLANNEL-POSTRTG", "1"); code != 0 {
		t.Fatalf("ChainExists: %d", code)
	}
	if _, code := ipt(t, "-t", "filter", "-S", "FLANNEL-FWD", "1"); code != 1 {
		t.Fatalf("ChainExists(missing): want 1, got %d", code)
	}
	// Exists before bootstrap: exit 1.
	if _, code := ipt(t, append([]string{"-t", "nat", "-C"}, masqRules[0]...)...); code != 1 {
		t.Fatalf("-C before bootstrap: want 1, got %d", code)
	}

	// Bootstrap through iptables-restore --noflush --wait.
	if _, errb, code := run(t, restoreCmd, payload("nat", withAction("-A", masqRules)), "--noflush", "--wait"); code != 0 {
		t.Fatalf("restore: %d %s", code, errb)
	}
	for _, r := range masqRules {
		if _, code := ipt(t, append([]string{"-t", "nat", "-C"}, r...)...); code != 0 {
			t.Errorf("-C %q after bootstrap: %d", r, code)
		}
	}
	// Equivalent spellings (host address, option order) are the same rule.
	if _, code := ipt(t, "-t", "nat", "-C", "FLANNEL-POSTRTG", "-d", "10.42.0.0/16", "-s", "10.42.1.0/24", "-m", "comment", "--comment", masq, "-j", "RETURN"); code != 0 {
		t.Errorf("-C reordered: %d", code)
	}

	st, err := b.GetTable(nft.FamilyIP, "nat")
	if err != nil || len(st.Rules) != len(masqRules) {
		t.Fatalf("nat table: %v, %d rules", err, len(st.Rules))
	}
	out, _ := ipt(t, "-t", "nat", "-S")
	if !strings.Contains(out, "-P POSTROUTING ACCEPT\n") || !strings.Contains(out, "-N FLANNEL-POSTRTG\n") ||
		!strings.Contains(out, `-A FLANNEL-POSTRTG -s 10.42.0.0/16 ! -d 224.0.0.0/4 -m comment --comment "flanneld masq" -j MASQUERADE --random-fully`) {
		t.Fatalf("-S:\n%s", out)
	}

	// Resync after a lease change: -D the old rule, -A the new one, atomically.
	resync := payload("nat", [][]string{
		append([]string{"-D"}, masqRules[2]...),
		{"-A", "FLANNEL-POSTRTG", "-s", "10.42.7.0/24", "-d", "10.42.0.0/16", "-m", "comment", "--comment", masq, "-j", "RETURN"},
	})
	if _, errb, code := run(t, restoreCmd, resync, "--noflush"); code != 0 {
		t.Fatalf("resync: %d %s", code, errb)
	}
	if _, code := ipt(t, append([]string{"-t", "nat", "-C"}, masqRules[2]...)...); code != 1 {
		t.Fatalf("deleted rule still present")
	}
	// A -D of a rule that is not there fails the whole payload.
	bad := payload("nat", [][]string{
		{"-A", "FLANNEL-POSTRTG", "-s", "10.42.9.0/24", "-j", "RETURN"},
		append([]string{"-D"}, masqRules[2]...),
	})
	if _, _, code := run(t, restoreCmd, bad, "--noflush"); code != 1 {
		t.Fatalf("bad payload: want 1, got %d", code)
	}
	if _, code := ipt(t, "-t", "nat", "-C", "FLANNEL-POSTRTG", "-s", "10.42.9.0/24", "-j", "RETURN"); code != 1 {
		t.Fatalf("failed payload leaked a rule")
	}

	// Forward rules in the filter table.
	fwd := payload("filter", [][]string{
		{"-A", "FORWARD", "-m", "comment", "--comment", "flanneld forward", "-j", "FLANNEL-FWD"},
		{"-A", "FLANNEL-FWD", "-s", "10.42.0.0/16", "-m", "comment", "--comment", "flanneld forward", "-j", "ACCEPT"},
	})
	if _, _, code := run(t, restoreCmd, fwd, "--noflush"); code == 0 {
		t.Fatalf("jump to a missing chain must fail")
	}
	if _, code := ipt(t, "-t", "filter", "-N", "FLANNEL-FWD"); code != 0 {
		t.Fatal("-N FLANNEL-FWD")
	}
	if _, errb, code := run(t, restoreCmd, fwd, "--noflush"); code != 0 {
		t.Fatalf("forward restore: %d %s", code, errb)
	}
}

func TestNftTranslation(t *testing.T) {
	r, err := parseRule([]string{"!", "-s", "10.42.0.5", "-d", "10.42.0.0/16", "-m", "mark", "--mark", "0x4000/0x4000", "-j", "MASQUERADE", "--random-fully"}, nft.FamilyIP)
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.nftText(nft.FamilyIP, func(string) bool { return false })
	if want := "ip saddr != 10.42.0.5/32 ip daddr 10.42.0.0/16 mark and 0x4000 == 0x4000 masquerade fully-random"; err != nil || text != want {
		t.Fatalf("nftText = %q, %v; want %q", text, err, want)
	}
	if _, err := nft.CompileRuleText(nft.FamilyIP, "nat", "c", text, nil); err != nil {
		t.Fatalf("compile %q: %v", text, err)
	}
	r, _ = parseRule([]string{"-d", "fd00::/8", "-j", "ACCEPT"}, nft.FamilyIP6)
	if text, _ = r.nftText(nft.FamilyIP6, nil); text != "ip6 daddr fd00::/8 accept" {
		t.Fatalf("ip6 = %q", text)
	}
}

func TestOutsideTheSubset(t *testing.T) {
	fake(t)
	for _, args := range [][]string{
		{"-t", "mangle", "-N", "KUBE-IPTABLES-HINT"}, // kubelet util chains
		{"-t", "nat", "-C", "OUTPUT", "-m", "conntrack", "--ctstate", "NEW", "-j", "KUBE-SERVICES"},
		{"-t", "nat", "-A", "POSTROUTING", "-p", "tcp", "-j", "ACCEPT"},
		{"-t", "nat", "-A", "POSTROUTING", "-j", "SNAT", "--to-source", "1.2.3.4"},
		{"-t", "nat", "-I", "POSTROUTING", "1", "-j", "ACCEPT"},
		{"-L"},
	} {
		if _, errb, code := run(t, iptablesCmd, "", args...); code != 2 || !strings.Contains(errb, "not supported") {
			t.Errorf("%q: want exit 2 'not supported', got %d %q", args, code, errb)
		}
	}
	if _, _, code := run(t, restoreCmd, "*nat\nCOMMIT\n"); code != 2 {
		t.Errorf("restore without --noflush: want 2, got %d", code)
	}
	// kube-proxy's startup presence check is answered (built-in chains exist).
	if out, _, code := run(t, iptablesCmd, "", "-w", "5", "-S", "POSTROUTING", "-t", "nat"); code != 0 || out != "-P POSTROUTING ACCEPT\n" {
		t.Errorf("presence check: %d %q", code, out)
	}
}
