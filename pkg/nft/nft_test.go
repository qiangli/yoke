package nft

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// The fixtures below are the exact line format sigs.k8s.io/knftables
// writes to `nft -f -` (objects.go writeOperation) for kube-proxy's
// nftables proxier and flannel's nftables traffic manager.

const kubeProxyFirstSync = `add table ip kube-proxy { comment "rules for kube-proxy" ; }
add chain ip kube-proxy filter-prerouting-pre-dnat { type filter hook prerouting priority -110 ; }
add chain ip kube-proxy filter-input { type filter hook input priority 0 ; }
add chain ip kube-proxy filter-forward { type filter hook forward priority 0 ; }
add chain ip kube-proxy filter-output { type filter hook output priority 0 ; }
add chain ip kube-proxy nat-prerouting { type nat hook prerouting priority -100 ; }
add chain ip kube-proxy nat-output { type nat hook output priority -100 ; }
add chain ip kube-proxy nat-postrouting { type nat hook postrouting priority 100 ; }
add chain ip kube-proxy services
add chain ip kube-proxy masquerading
add chain ip kube-proxy cluster-ips-check
add chain ip kube-proxy firewall-check
add chain ip kube-proxy service-endpoints-check
add chain ip kube-proxy nodeport-endpoints-check
add chain ip kube-proxy reject-chain { comment "helper for @no-endpoint-services / @no-endpoint-nodeports" ; }
add set ip kube-proxy cluster-ips { type ipv4_addr ; comment "Active ClusterIPs" ; }
add set ip kube-proxy nodeport-ips { type ipv4_addr ; comment "IPs that accept NodePort traffic" ; }
add set ip kube-proxy hairpin-connections { type ipv4_addr . ipv4_addr . inet_proto . inet_service ; comment "service hairpin connections" ; }
add map ip kube-proxy service-ips { type ipv4_addr . inet_proto . inet_service : verdict ; comment "ClusterIP, ExternalIP and LoadBalancer IP traffic" ; }
add map ip kube-proxy service-nodeports { type inet_proto . inet_service : verdict ; comment "NodePort traffic" ; }
add map ip kube-proxy no-endpoint-services { type ipv4_addr . inet_proto . inet_service : verdict ; comment "vmap to drop or reject packets to services with no endpoints" ; }
add map ip kube-proxy no-endpoint-nodeports { type inet_proto . inet_service : verdict ; comment "vmap to drop or reject packets to service nodeports with no endpoints" ; }
add map ip kube-proxy firewall-ips { type ipv4_addr . inet_proto . inet_service : verdict ; comment "destinations that are subject to LoadBalancerSourceRanges" ; }
add counter ip kube-proxy localhost-nodeport-rejected-tcp
add chain ip kube-proxy localhost-nodeport-reject-tcp
flush chain ip kube-proxy filter-input
flush chain ip kube-proxy filter-forward
flush chain ip kube-proxy masquerading
add rule ip kube-proxy filter-prerouting-pre-dnat ct state new jump firewall-check
add rule ip kube-proxy filter-input ct state new jump nodeport-endpoints-check
add rule ip kube-proxy filter-input ct state new jump service-endpoints-check
add rule ip kube-proxy filter-forward ct state new jump service-endpoints-check
add rule ip kube-proxy filter-forward ct state new jump cluster-ips-check
add rule ip kube-proxy filter-output ct state new jump service-endpoints-check
add rule ip kube-proxy filter-output ct state new jump cluster-ips-check
add rule ip kube-proxy nat-prerouting jump services
add rule ip kube-proxy nat-output jump services
add rule ip kube-proxy nat-postrouting jump masquerading
add rule ip kube-proxy masquerading mark and 0x004000 != 0 mark set mark xor 0x004000 masquerade fully-random
add rule ip kube-proxy masquerading ct status dnat ip saddr . ip daddr . meta l4proto . th dport @hairpin-connections masquerade fully-random
add rule ip kube-proxy cluster-ips-check ip daddr @cluster-ips reject comment "Reject traffic to invalid ports of ClusterIPs"
add rule ip kube-proxy cluster-ips-check ip daddr { 10.43.0.0/16 } drop comment "Drop traffic to unallocated ClusterIPs"
add rule ip kube-proxy reject-chain reject
add rule ip kube-proxy service-endpoints-check ip daddr . meta l4proto . th dport vmap @no-endpoint-services
add rule ip kube-proxy nodeport-endpoints-check ip daddr != 127.0.0.0/8 meta l4proto . th dport vmap @no-endpoint-nodeports
add rule ip kube-proxy localhost-nodeport-reject-tcp counter name "localhost-nodeport-rejected-tcp" reject
add rule ip kube-proxy firewall-check ip daddr . meta l4proto . th dport vmap @firewall-ips
add rule ip kube-proxy services ip daddr . meta l4proto . th dport vmap @service-ips
add rule ip kube-proxy services ip daddr @cluster-ips ip saddr != 10.42.0.0/16 mark set mark or 0x004000 comment "masquerade clusterIP traffic from outside cluster"
add rule ip kube-proxy services fib daddr type local ip daddr != 127.0.0.0/8 meta l4proto . th dport vmap @service-nodeports
add chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
add chain ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80
add chain ip kube-proxy endpoint-SGOXE6O3-ns1/svc1/tcp/p80__10.42.2.7/80
add chain ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80
add set ip kube-proxy affinity-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 { type ipv4_addr ; flags dynamic,timeout ; timeout 10800s ; }
add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 ip saddr 10.42.1.5 mark set mark or 0x004000 comment "masquerade hairpin traffic"
add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 update @affinity-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 { ip saddr }
add rule ip kube-proxy endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 meta l4proto tcp dnat to 10.42.1.5:80
add rule ip kube-proxy endpoint-SGOXE6O3-ns1/svc1/tcp/p80__10.42.2.7/80 meta l4proto tcp dnat to 10.42.2.7:80
add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip daddr 10.43.0.41 tcp dport 80 ip saddr != 10.42.0.0/16 mark set mark or 0x004000
add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 ip saddr @affinity-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 goto endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80
add rule ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80 numgen random mod 2 vmap { 0 : goto endpoint-5OJB2KTY-ns1/svc1/tcp/p80__10.42.1.5/80 , 1 : goto endpoint-SGOXE6O3-ns1/svc1/tcp/p80__10.42.2.7/80 }
add rule ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80 fib saddr type local mark set mark or 0x004000 comment "masquerade local traffic"
add rule ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80 fib saddr type local goto service-ULMVA6XW-ns1/svc1/tcp/p80 comment "short-circuit local traffic"
add rule ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80 goto service-ULMVA6XW-ns1/svc1/tcp/p80
add element ip kube-proxy cluster-ips { 10.43.0.41 }
add element ip kube-proxy service-ips { 10.43.0.41 . tcp . 80 : goto service-ULMVA6XW-ns1/svc1/tcp/p80 }
add element ip kube-proxy service-nodeports { tcp . 30080 : goto external-ULMVA6XW-ns1/svc1/tcp/p80 }
add element ip kube-proxy no-endpoint-services { 10.43.0.99 . udp . 53 comment "ns2/svc2:dns" : goto reject-chain }
add element ip kube-proxy hairpin-connections { 10.42.1.5 . 10.42.1.5 . tcp . 80 }
`

func mustRun(t *testing.T, b Backend, script string) {
	t.Helper()
	if err := Run(b, script, false); err != nil {
		t.Fatalf("Run: %v\n%s", err, ErrorText(err))
	}
}

func listJSON(t *testing.T, b Backend, a ListArgs) map[string]any {
	t.Helper()
	out, err := ListJSON(b, a)
	if err != nil {
		t.Fatalf("ListJSON(%+v): %v", a, err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("bad JSON %s: %v", out, err)
	}
	return v
}

// objects mimics knftables' getJSONObjects.
func objects(t *testing.T, v map[string]any, typ string) []map[string]any {
	t.Helper()
	arr := v["nftables"].([]any)
	meta := arr[0].(map[string]any)["metainfo"].(map[string]any)
	if meta["json_schema_version"].(float64) != 1 {
		t.Fatalf("bad metainfo %v", meta)
	}
	var out []map[string]any
	for _, o := range arr[1:] {
		if x, ok := o.(map[string]any)[typ]; ok {
			out = append(out, x.(map[string]any))
		}
	}
	return out
}

func names(objs []map[string]any) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o["name"].(string))
	}
	return out
}

func TestKubeProxySyncCycle(t *testing.T) {
	b := NewMemBackend()
	if err := Run(b, kubeProxyFirstSync, true); err != nil {
		t.Fatalf("--check of first sync: %v", err)
	}
	if _, err := b.GetTable(FamilyIP, "kube-proxy"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("--check must not write; GetTable err=%v", err)
	}
	mustRun(t, b, kubeProxyFirstSync)

	terse := listJSON(t, b, ListArgs{What: "table", Family: FamilyIP, Table: "kube-proxy", Terse: true})
	if got := len(objects(t, terse, "chain")); got != 19 {
		t.Fatalf("chains = %d, want 19", got)
	}
	if got := names(objects(t, terse, "map")); !reflect.DeepEqual(got, []string{"firewall-ips", "no-endpoint-nodeports", "no-endpoint-services", "service-ips", "service-nodeports"}) {
		t.Fatalf("maps = %v", got)
	}
	for _, s := range objects(t, terse, "set") {
		if _, ok := s["elem"]; ok {
			t.Fatalf("--terse listed elements: %v", s)
		}
	}

	m := listJSON(t, b, ListArgs{What: "map", Family: FamilyIP, Table: "kube-proxy", Name: "service-ips"})
	el := objects(t, m, "map")[0]["elem"].([]any)
	want := `[[{"concat":["10.43.0.41","tcp",80]},{"goto":{"target":"service-ULMVA6XW-ns1/svc1/tcp/p80"}}]]`
	if got, _ := json.Marshal(el); string(got) != want {
		t.Fatalf("service-ips elem = %s, want %s", got, want)
	}
	m = listJSON(t, b, ListArgs{What: "map", Family: FamilyIP, Table: "kube-proxy", Name: "no-endpoint-services"})
	want = `[[{"elem":{"comment":"ns2/svc2:dns","val":{"concat":["10.43.0.99","udp",53]}}},{"goto":{"target":"reject-chain"}}]]`
	if got, _ := json.Marshal(objects(t, m, "map")[0]["elem"]); string(got) != want {
		t.Fatalf("no-endpoint-services elem = %s, want %s", got, want)
	}
	s := listJSON(t, b, ListArgs{What: "set", Family: FamilyIP, Table: "kube-proxy", Name: "cluster-ips"})
	if got, _ := json.Marshal(objects(t, s, "set")[0]["elem"]); string(got) != `["10.43.0.41"]` {
		t.Fatalf("cluster-ips elem = %s", got)
	}
	aff := listJSON(t, b, ListArgs{What: "table", Family: FamilyIP, Table: "kube-proxy", Terse: true})
	for _, s := range objects(t, aff, "set") {
		if strings.HasPrefix(s["name"].(string), "affinity-") {
			if s["timeout"].(float64) != 10800 || !reflect.DeepEqual(s["flags"], []any{"timeout", "dynamic"}) {
				t.Fatalf("affinity set = %v", s)
			}
		}
	}

	// Second (partial) sync: churn elements, flush and drop stale objects.
	mustRun(t, b, `delete element ip kube-proxy service-ips { 10.43.0.41 . tcp . 80 }
delete element ip kube-proxy cluster-ips { 10.43.0.41 }
flush chain ip kube-proxy service-ULMVA6XW-ns1/svc1/tcp/p80
flush map ip kube-proxy no-endpoint-services
flush set ip kube-proxy hairpin-connections
add element ip kube-proxy cluster-ips { 10.43.0.42 }
`)
	mustRun(t, b, `flush chain ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80
delete chain ip kube-proxy external-ULMVA6XW-ns1/svc1/tcp/p80
delete set ip kube-proxy nodeport-ips
`)
	s = listJSON(t, b, ListArgs{What: "set", Family: FamilyIP, Table: "kube-proxy", Name: "cluster-ips"})
	if got, _ := json.Marshal(objects(t, s, "set")[0]["elem"]); string(got) != `["10.43.0.42"]` {
		t.Fatalf("cluster-ips after churn = %s", got)
	}

	// Deleting an absent element is ENOENT, as the kernel reports it, and
	// the whole transaction is discarded.
	err := Run(b, "add element ip kube-proxy cluster-ips { 10.43.0.50 }\ndelete element ip kube-proxy cluster-ips { 10.43.0.41 }\n", false)
	if !errors.Is(err, syscall.ENOENT) || !strings.Contains(strings.SplitN(ErrorText(err), "\n", 2)[0], "No such file or directory") {
		t.Fatalf("want ENOENT on first stderr line, got %v / %q", err, ErrorText(err))
	}
	s = listJSON(t, b, ListArgs{What: "set", Family: FamilyIP, Table: "kube-proxy", Name: "cluster-ips"})
	if got, _ := json.Marshal(objects(t, s, "set")[0]["elem"]); string(got) != `["10.43.0.42"]` {
		t.Fatalf("failed transaction leaked: %s", got)
	}

	ctr := listJSON(t, b, ListArgs{What: "counters", Family: FamilyIP, Table: "kube-proxy"})
	if c := objects(t, ctr, "counter"); len(c) != 1 || c[0]["name"] != "localhost-nodeport-rejected-tcp" || c[0]["packets"].(float64) != 0 {
		t.Fatalf("counters = %v", c)
	}

	// Cleanup path, and the probe knftables.New runs.
	mustRun(t, b, "delete table ip kube-proxy\n")
	if err := Run(b, "delete table ip kube-proxy\n", false); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("delete of absent table: %v", err)
	}
	if err := Run(b, "add table ip kube-proxy { comment \"test\" ; }\n", true); err != nil {
		t.Fatalf("knftables probe: %v", err)
	}
}

func TestKubeProxyIPv6AndRuleChecks(t *testing.T) {
	b := NewMemBackend()
	mustRun(t, b, `add table ip6 kube-proxy { comment "rules for kube-proxy" ; }
add chain ip6 kube-proxy services
add chain ip6 kube-proxy endpoint-A
add chain ip6 kube-proxy svc
add set ip6 kube-proxy cluster-ips { type ipv6_addr ; comment "Active ClusterIPs" ; }
add map ip6 kube-proxy service-ips { type ipv6_addr . inet_proto . inet_service : verdict ; }
add rule ip6 kube-proxy services ip6 daddr . meta l4proto . th dport vmap @service-ips
add rule ip6 kube-proxy endpoint-A meta l4proto tcp dnat to [fd00:10:42::5]:80
add rule ip6 kube-proxy svc meta l4proto tcp dnat ip6 addr . port to numgen random mod 2 map { 0 : fd00:10:42::5 . 80 , 1 : fd00:10:42::6 . 80 }
add element ip6 kube-proxy service-ips { fd00:10:43::a . udp . 53 : goto endpoint-A }
`)
	m := listJSON(t, b, ListArgs{What: "map", Family: FamilyIP6, Table: "kube-proxy", Name: "service-ips"})
	if got, _ := json.Marshal(objects(t, m, "map")[0]["elem"]); string(got) != `[[{"concat":["fd00:10:43::a","udp",53]},{"goto":{"target":"endpoint-A"}}]]` {
		t.Fatalf("ip6 elem = %s", got)
	}
	for _, bad := range []string{
		"add rule ip6 kube-proxy svc ip daddr 1.2.3.4 drop",               // wrong family
		"add rule ip6 kube-proxy svc ip6 daddr @nope drop",                // missing set
		"add rule ip6 kube-proxy svc ip6 daddr @service-ips drop",         // map used as set
		"add rule ip6 kube-proxy svc meta l4proto vmap @service-ips",      // key mismatch
		"add element ip6 kube-proxy service-ips { fd00::1 . tcp : drop }", // short key
		"add rule ip6 kube-proxy svc log prefix \"x\"",                    // outside the subset
		"insert rule ip6 kube-proxy svc drop",                             // verb outside the subset
	} {
		if err := Run(b, bad+"\n", false); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestFlannelNFTablesRules(t *testing.T) {
	b := NewMemBackend()
	mustRun(t, b, `add table ip flannel-ipv4 { comment "rules for flannel-ipv4" ; }
add chain ip flannel-ipv4 postrtg { type nat hook postrouting priority 100 ; comment "chain to manage traffic masquerading by flannel" ; }
flush chain ip flannel-ipv4 postrtg
add rule ip flannel-ipv4 postrtg meta mark 0x4000 return
add rule ip flannel-ipv4 postrtg ip saddr 10.42.0.0/24 ip daddr 10.42.0.0/16 return
add rule ip flannel-ipv4 postrtg ip saddr 10.42.0.0/16 ip daddr 10.42.0.0/24 return
add rule ip flannel-ipv4 postrtg ip saddr != 10.42.0.0/24 ip daddr 10.42.0.0/16 return
add rule ip flannel-ipv4 postrtg ip saddr 10.42.0.0/16 ip daddr != 224.0.0.0/4 masquerade fully-random
add rule ip flannel-ipv4 postrtg ip saddr != 10.42.0.0/16 ip daddr 10.42.0.0/16 masquerade fully-random
add chain ip flannel-ipv4 forward { type filter hook forward priority 0 ; comment "chain to accept flannel traffic" ; }
flush chain ip flannel-ipv4 forward
add rule ip flannel-ipv4 forward ip saddr 10.42.0.0/16 accept
add rule ip flannel-ipv4 forward ip daddr 10.42.0.0/16 accept
`)
	v := listJSON(t, b, ListArgs{What: "chain", Family: FamilyIP, Table: "flannel-ipv4", Name: "postrtg"})
	if c := objects(t, v, "chain"); len(c) != 1 || c[0]["type"] != "nat" || c[0]["hook"] != "postrouting" || c[0]["prio"].(float64) != 100 {
		t.Fatalf("chain = %v", c)
	}
	if r := objects(t, v, "rule"); len(r) != 6 {
		t.Fatalf("rules = %d", len(r))
	}
}

func ne32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }

func compile(t *testing.T, fam Family, rule string, sets map[string]*SetSpec) *RuleSpec {
	t.Helper()
	toks, err := lex(rule)
	if err != nil {
		t.Fatal(err)
	}
	r, err := CompileRule(fam, "t", "c", toks, func(n string) (*SetSpec, error) {
		if s, ok := sets[n]; ok {
			return s, nil
		}
		return nil, notFound("set %s", n)
	})
	if err != nil {
		t.Fatalf("%q: %v", rule, err)
	}
	return r
}

// The expected expressions are the register-level shapes `nft
// --debug=netlink` documents for each statement.
func TestCompileShapes(t *testing.T) {
	ipt := dataTypes["ipv4_addr"]
	vm := &SetSpec{Name: "service-ips", Keys: []DataType{ipt, dataTypes["inet_proto"], dataTypes["inet_service"]}, Data: []DataType{dataTypes["verdict"]}}
	vm6 := &SetSpec{Name: "service-ips", Keys: []DataType{dataTypes["ipv6_addr"], dataTypes["inet_proto"], dataTypes["inet_service"]}, Data: []DataType{dataTypes["verdict"]}}
	aff := &SetSpec{Name: "aff", Keys: []DataType{ipt}, Dynamic: true, HasTimeout: true}
	zero4 := make([]byte, 4)

	cases := []struct {
		fam  Family
		rule string
		sets map[string]*SetSpec
		want []Expr
	}{
		{FamilyIP, "ip daddr . meta l4proto . th dport vmap @service-ips", map[string]*SetSpec{"service-ips": vm}, []Expr{
			Payload{BaseNetwork, 16, 4, 1}, Meta{Key: "l4proto", DReg: 9}, Payload{BaseTransport, 2, 2, 10},
			Lookup{SReg: 1, DReg: 0, HasDReg: true, Set: "service-ips"},
		}},
		{FamilyIP6, "ip6 daddr . meta l4proto . th dport vmap @service-ips", map[string]*SetSpec{"service-ips": vm6}, []Expr{
			Payload{BaseNetwork, 24, 16, 1}, Meta{Key: "l4proto", DReg: 12}, Payload{BaseTransport, 2, 2, 13},
			Lookup{SReg: 1, DReg: 0, HasDReg: true, Set: "service-ips"},
		}},
		{FamilyIP, "mark and 0x004000 != 0 mark set mark xor 0x004000 masquerade fully-random", nil, []Expr{
			Meta{Key: "mark", DReg: 1}, Bitwise{1, 1, 4, ne32(0x4000), zero4}, Cmp{"neq", 1, zero4},
			Meta{Key: "mark", DReg: 1}, Bitwise{1, 1, 4, ne32(0xffffffff), ne32(0x4000)}, Meta{Key: "mark", Set: true, SReg: 1},
			Masq{FullyRandom: true},
		}},
		{FamilyIP, "mark set mark or 0x4000", nil, []Expr{
			Meta{Key: "mark", DReg: 1}, Bitwise{1, 1, 4, ne32(^uint32(0x4000)), ne32(0x4000)}, Meta{Key: "mark", Set: true, SReg: 1},
		}},
		{FamilyIP, "ct state new jump x", nil, []Expr{
			Ct{Key: "state", DReg: 1}, Bitwise{1, 1, 4, ne32(8), zero4}, Cmp{"neq", 1, zero4}, VerdictExpr{Verdict{"jump", "x"}},
		}},
		{FamilyIP, "fib saddr type local goto svc", nil, []Expr{
			Fib{SAddr: true, DReg: 1}, Cmp{"eq", 1, ne32(2)}, VerdictExpr{Verdict{"goto", "svc"}},
		}},
		{FamilyIP, "ip saddr != 10.42.0.0/16 return", nil, []Expr{
			Payload{BaseNetwork, 12, 4, 1}, Bitwise{1, 1, 4, []byte{255, 255, 0, 0}, zero4}, Cmp{"neq", 1, []byte{10, 42, 0, 0}},
			VerdictExpr{Verdict{Kind: "return"}},
		}},
		{FamilyIP, "meta l4proto tcp dnat to 10.42.1.5:80", nil, []Expr{
			Meta{Key: "l4proto", DReg: 1}, Cmp{"eq", 1, []byte{6}},
			Immediate{1, []byte{10, 42, 1, 5}}, Immediate{2, []byte{0, 80}}, Nat{"dnat", FamilyIP, 1, 2},
		}},
		{FamilyIP6, "dnat to [fd00::5]:8080", nil, []Expr{
			Immediate{1, []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}}, Immediate{2, []byte{0x1f, 0x90}}, Nat{"dnat", FamilyIP6, 1, 2},
		}},
		{FamilyIP, "update @aff { ip saddr }", map[string]*SetSpec{"aff": aff}, []Expr{
			Payload{BaseNetwork, 12, 4, 1}, Dynset{"update", 1, "aff"},
		}},
		{FamilyIP, `counter name "localhost-nodeport-rejected-tcp" reject`, nil, []Expr{
			ObjRef{"localhost-nodeport-rejected-tcp"}, Reject{0, 3},
		}},
		{FamilyIP6, "reject", nil, []Expr{Reject{0, 4}}},
		// A one-element anonymous set is a plain (prefix) comparison.
		{FamilyIP, "ip daddr { 10.43.0.0/16 } drop", nil, []Expr{
			Payload{BaseNetwork, 16, 4, 1}, Bitwise{1, 1, 4, []byte{255, 255, 0, 0}, zero4}, Cmp{"eq", 1, []byte{10, 43, 0, 0}},
			VerdictExpr{Verdict{Kind: "drop"}},
		}},
		{FamilyIP, `iifname "veth*" accept`, nil, []Expr{
			Meta{Key: "iifname", DReg: 1}, Cmp{"eq", 1, []byte("veth")}, VerdictExpr{Verdict{Kind: "accept"}},
		}},
		{FamilyIP, "tcp dport 80 counter accept", nil, []Expr{
			Meta{Key: "l4proto", DReg: 1}, Cmp{"eq", 1, []byte{6}}, Payload{BaseTransport, 2, 2, 1}, Cmp{"eq", 1, []byte{0, 80}},
			Counter{}, VerdictExpr{Verdict{Kind: "accept"}},
		}},
	}
	for _, tc := range cases {
		r := compile(t, tc.fam, tc.rule, tc.sets)
		if !reflect.DeepEqual(r.Exprs, tc.want) {
			t.Errorf("%q:\n got %#v\nwant %#v", tc.rule, r.Exprs, tc.want)
		}
	}
}

func TestCompileAnonymousSets(t *testing.T) {
	r := compile(t, FamilyIP, "meta l4proto tcp dnat ip addr . port to numgen random mod 2 map { 0 : 10.42.1.5 . 80 , 1 : 10.42.2.7 . 8080 }", nil)
	want := []Expr{
		Meta{Key: "l4proto", DReg: 1}, Cmp{"eq", 1, []byte{6}},
		Numgen{Mod: 2, DReg: 1}, Lookup{SReg: 1, DReg: 1, HasDReg: true, Anon: 0}, Nat{"dnat", FamilyIP, 1, 9},
	}
	if !reflect.DeepEqual(r.Exprs, want) {
		t.Fatalf("got %#v", r.Exprs)
	}
	a := r.AnonSets[0]
	if !a.Spec.Anonymous || !a.Spec.Constant || len(a.Elements) != 2 ||
		!reflect.DeepEqual(a.Elements[1], Element{Key: ne32(1), Data: []byte{10, 42, 2, 7, 0x1f, 0x90, 0, 0}}) {
		t.Fatalf("anon map = %+v", a)
	}

	r = compile(t, FamilyIP, "numgen random mod 2 vmap { 0 : goto a , 1 : goto b }", nil)
	if e := r.AnonSets[0].Elements; len(e) != 2 || *e[1].Verdict != (Verdict{"goto", "b"}) || !r.AnonSets[0].Spec.IsVerdictMap() {
		t.Fatalf("anon vmap = %+v", r.AnonSets[0])
	}

	// Prefixes become an interval set with start / end-exclusive elements,
	// merged and sorted; a negated match inverts the lookup.
	r = compile(t, FamilyIP, "ip saddr != { 192.168.1.0/24, 10.0.0.1, 192.168.0.0/24 } drop", nil)
	if l := r.Exprs[1].(Lookup); !l.Invert {
		t.Fatalf("lookup not inverted: %#v", l)
	}
	s := r.AnonSets[0]
	wantEls := []Element{
		{Key: []byte{10, 0, 0, 1}}, {Key: []byte{10, 0, 0, 2}, IntervalEnd: true},
		{Key: []byte{192, 168, 0, 0}}, {Key: []byte{192, 168, 2, 0}, IntervalEnd: true},
	}
	if !s.Spec.Interval || !reflect.DeepEqual(s.Elements, wantEls) {
		t.Fatalf("interval set = %+v", s)
	}
	r = compile(t, FamilyIP, "ip daddr { 10.43.0.1, 10.43.0.2 } drop", nil)
	if r.AnonSets[0].Spec.Interval || len(r.AnonSets[0].Elements) != 2 {
		t.Fatalf("host-only set should be a plain hash set: %+v", r.AnonSets[0])
	}
}

func TestParse(t *testing.T) {
	cmds, err := Parse("# comment\nadd chain ip t c { type nat hook output priority dstnat+5 ; policy drop ; comment \"a \\\"q\\\"\" ; }; flush chain ip t c\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 || cmds[0].Chain.Priority != -95 || cmds[0].Chain.Policy != "drop" || cmds[0].Chain.Comment != `a "q"` || cmds[1].Verb != "flush" {
		t.Fatalf("cmds = %+v %+v", cmds[0], cmds[0].Chain)
	}
	for in, want := range map[string]int64{"10800s": 10800, "3h": 10800, "1d2h": 93600, "90": 90} {
		d, err := parseDuration(in)
		if err != nil || int64(d.Seconds()) != want {
			t.Errorf("parseDuration(%q) = %v, %v", in, d, err)
		}
	}
	for _, bad := range []string{"add table ip", "add table foo t", "add chain ip t c { type nat hook output }", "add set ip t s { type bogus ; }", "add rule ip t c handle 3 drop", "add table ip t {"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): want error", bad)
		}
	}
}
