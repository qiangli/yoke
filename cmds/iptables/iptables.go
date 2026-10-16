// Package iptablescmd implements iptables/ip6tables and
// iptables-restore/ip6tables-restore for exactly the argv k3s's embedded
// flannel drives (flannel pkg/trafficmngr/iptables over
// github.com/coreos/go-iptables): k3s hardcodes flannel's iptables traffic
// manager, and no k3s option switches it to nftables. Rules are written as
// native nftables (pkg/nft, netlink) into the tables iptables-nft uses
// ("ip nat", "ip filter", …), each rule carrying its canonical iptables
// rulespec as its nft comment — which is how -C and -D find it again.
//
//	iptables --version
//	iptables [-w [N]] -t nat|filter -N|-F|-X CHAIN
//	iptables [-w [N]] -t nat|filter -S [CHAIN [N]]
//	iptables [-w [N]] -t nat|filter -A|-C|-D CHAIN RULESPEC
//	iptables-restore --noflush [-w [N]] [--counters] [-T TABLE] < payload
//
// RULESPEC: [!] -s ADDR[/LEN], [!] -d ADDR[/LEN], -m comment --comment TEXT,
// -m mark --mark VALUE[/MASK], -j RETURN|ACCEPT|DROP|MASQUERADE
// [--random-fully]|CHAIN. Anything else — other tables, matches, targets —
// exits 2 naming what is unsupported. Linux only.
package iptablescmd

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/nft"
)

// Version is the iptables release whose CLI this emulates: >= 1.6.2 makes
// go-iptables use --wait and --random-fully, and flannel's iptables-restore
// use --wait.
const Version = "1.8.10"

var (
	iptablesCmd  = &tool.Tool{Name: "iptables", Synopsis: "IPv4 packet filter/NAT rules (flannel's subset; nftables backend).", Usage: usage("iptables")}
	ip6tablesCmd = &tool.Tool{Name: "ip6tables", Synopsis: "IPv6 packet filter/NAT rules (flannel's subset; nftables backend).", Usage: usage("ip6tables")}
	restoreCmd   = &tool.Tool{Name: "iptables-restore", Synopsis: "Apply an iptables-restore payload (--noflush; flannel's subset).", Usage: "iptables-restore --noflush [-w [N]] [--counters] [-T TABLE] < FILE"}
	restore6Cmd  = &tool.Tool{Name: "ip6tables-restore", Synopsis: "Apply an ip6tables-restore payload (--noflush; flannel's subset).", Usage: "ip6tables-restore --noflush [-w [N]] [--counters] [-T TABLE] < FILE"}
)

func usage(n string) string {
	return n + " [-w [N]] [-t TABLE] -A|-C|-D CHAIN RULESPEC\n   or: " + n + " [-w [N]] [-t TABLE] -N|-F|-X CHAIN\n   or: " +
		n + " [-w [N]] [-t TABLE] -S [CHAIN [N]]\n   or: " + n + " --version"
}

func init() {
	iptablesCmd.Run = func(rc *tool.RunContext, a []string) int { return runIptables(rc, iptablesCmd, nft.FamilyIP, a) }
	ip6tablesCmd.Run = func(rc *tool.RunContext, a []string) int { return runIptables(rc, ip6tablesCmd, nft.FamilyIP6, a) }
	restoreCmd.Run = func(rc *tool.RunContext, a []string) int { return runRestore(rc, restoreCmd, nft.FamilyIP, a) }
	restore6Cmd.Run = func(rc *tool.RunContext, a []string) int { return runRestore(rc, restore6Cmd, nft.FamilyIP6, a) }
	for _, t := range []*tool.Tool{iptablesCmd, ip6tablesCmd, restoreCmd, restore6Cmd} {
		tool.Register(t)
	}
}

// newBackend is swapped by tests for an in-memory backend.
var newBackend = nft.NewNetlink

// builtin base chains per table, as iptables-nft creates them.
var builtins = map[string]map[string]nft.ChainSpec{
	"nat": {
		"PREROUTING":  {Type: "nat", Hook: "prerouting", Priority: -100},
		"INPUT":       {Type: "nat", Hook: "input", Priority: 100},
		"OUTPUT":      {Type: "nat", Hook: "output", Priority: -100},
		"POSTROUTING": {Type: "nat", Hook: "postrouting", Priority: 100},
	},
	"filter": {
		"INPUT":   {Type: "filter", Hook: "input", Priority: 0},
		"FORWARD": {Type: "filter", Hook: "forward", Priority: 0},
		"OUTPUT":  {Type: "filter", Hook: "output", Priority: 0},
	},
}

// usageErr is a command-line error (exit 2); other failures exit 1.
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

func unsupported(format string, a ...any) error {
	return usageErr{fmt.Sprintf(format, a...) + " is not supported by this iptables (flannel subset over nftables)"}
}

// rule is a parsed RULESPEC.
type rule struct {
	src, dst       string
	srcNot, dstNot bool
	comment        string
	hasComment     bool
	mark           string // VALUE[/MASK]
	target         string
	randomFully    bool
}

func normAddr(s string, fam nft.Family) (string, error) {
	if !strings.Contains(s, "/") {
		if fam == nft.FamilyIP6 {
			s += "/128"
		} else {
			s += "/32"
		}
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || p.Addr().Is4() != (fam == nft.FamilyIP) {
		return "", usageErr{fmt.Sprintf("host/network %q not found", s)}
	}
	return p.Masked().String(), nil
}

// parseRule parses RULESPEC tokens.
func parseRule(args []string, fam nft.Family) (*rule, error) {
	r := &rule{}
	not := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", usageErr{fmt.Sprintf("option %q requires an argument", a)}
			}
			i++
			return args[i], nil
		}
		switch a {
		case "!":
			not = true
			continue
		case "-s", "--source", "-d", "--destination":
			v, err := next()
			if err != nil {
				return nil, err
			}
			if v, err = normAddr(v, fam); err != nil {
				return nil, err
			}
			if a == "-s" || a == "--source" {
				r.src, r.srcNot = v, not
			} else {
				r.dst, r.dstNot = v, not
			}
		case "-m", "--match":
			m, err := next()
			if err != nil {
				return nil, err
			}
			if not {
				return nil, unsupported("! -m")
			}
			switch m {
			case "comment":
				if i+2 >= len(args) || args[i+1] != "--comment" {
					return nil, usageErr{"comment match requires --comment"}
				}
				r.comment, r.hasComment = args[i+2], true
				i += 2
			case "mark":
				if i+2 >= len(args) || args[i+1] != "--mark" {
					return nil, unsupported("-m mark without --mark")
				}
				r.mark = args[i+2]
				i += 2
			default:
				return nil, unsupported("match %q", m)
			}
		case "-j", "--jump":
			v, err := next()
			if err != nil {
				return nil, err
			}
			r.target = v
		case "--random-fully":
			if r.target != "MASQUERADE" {
				return nil, usageErr{"--random-fully needs -j MASQUERADE"}
			}
			r.randomFully = true
		default:
			return nil, unsupported("option %q", a)
		}
		if not && a != "-s" && a != "--source" && a != "-d" && a != "--destination" {
			return nil, unsupported("! before %q", a)
		}
		not = false
	}
	if not {
		return nil, usageErr{"trailing !"}
	}
	return r, nil
}

// canonical renders the rule in a fixed order; it is stored as the nft
// rule comment and compared by -C / -D.
func (r *rule) canonical() string {
	var b []string
	if r.src != "" {
		if r.srcNot {
			b = append(b, "!")
		}
		b = append(b, "-s", r.src)
	}
	if r.dst != "" {
		if r.dstNot {
			b = append(b, "!")
		}
		b = append(b, "-d", r.dst)
	}
	if r.mark != "" {
		b = append(b, "-m", "mark", "--mark", r.mark)
	}
	if r.hasComment {
		b = append(b, "-m", "comment", "--comment", strconv.Quote(r.comment))
	}
	if r.target != "" {
		b = append(b, "-j", r.target)
	}
	if r.randomFully {
		b = append(b, "--random-fully")
	}
	return strings.Join(b, " ")
}

// nftText translates the rule into the nft rule language.
func (r *rule) nftText(fam nft.Family, chainExists func(string) bool) (string, error) {
	var b []string
	af := string(fam)
	if r.src != "" {
		b = append(b, af, "saddr")
		if r.srcNot {
			b = append(b, "!=")
		}
		b = append(b, r.src)
	}
	if r.dst != "" {
		b = append(b, af, "daddr")
		if r.dstNot {
			b = append(b, "!=")
		}
		b = append(b, r.dst)
	}
	if r.mark != "" {
		v, m, masked := strings.Cut(r.mark, "/")
		if masked {
			b = append(b, "mark", "and", m, "==", v)
		} else {
			b = append(b, "meta", "mark", v)
		}
	}
	switch r.target {
	case "":
	case "RETURN", "ACCEPT", "DROP":
		b = append(b, strings.ToLower(r.target))
	case "MASQUERADE":
		b = append(b, "masquerade")
		if r.randomFully {
			b = append(b, "fully-random")
		}
	default:
		if strings.ToUpper(r.target) == r.target && !chainExists(r.target) {
			return "", unsupported("target %q", r.target)
		}
		b = append(b, "jump", r.target)
	}
	return strings.Join(b, " "), nil
}

// state is the table's live chains and rules plus this invocation's batch.
type state struct {
	b      nft.Backend
	fam    nft.Family
	table  string
	exists bool                      // the nft table exists
	chains map[string]bool           // materialized chains
	rules  map[string][]nft.RuleSpec // per chain
}

func load(b nft.Backend, fam nft.Family, table string) (*state, error) {
	if builtins[table] == nil {
		return nil, unsupported("table %q", table)
	}
	s := &state{b: b, fam: fam, table: table, chains: map[string]bool{}, rules: map[string][]nft.RuleSpec{}}
	st, err := b.GetTable(fam, table)
	if errors.Is(err, syscall.ENOENT) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.exists = true
	for _, c := range st.Chains {
		s.chains[c.Name] = true
	}
	for _, r := range st.Rules {
		s.rules[r.Chain] = append(s.rules[r.Chain], r)
	}
	return s, nil
}

func (s *state) isBuiltin(chain string) bool { _, ok := builtins[s.table][chain]; return ok }
func (s *state) has(chain string) bool       { return s.chains[chain] || s.isBuiltin(chain) }

// ensure queues creation of the table and (built-in) chain if needed.
func (s *state) ensure(chain string) error {
	if !s.exists {
		s.b.AddTable(s.fam, s.table, "", false)
		s.exists = true
	}
	if s.chains[chain] {
		return nil
	}
	spec, ok := builtins[s.table][chain]
	if !ok {
		return fmt.Errorf("No chain/target/match by that name")
	}
	spec.Family, spec.Table, spec.Name = s.fam, s.table, chain
	s.b.AddChain(&spec, false)
	s.chains[chain] = true
	return nil
}

func (s *state) newChain(chain string) error {
	if s.has(chain) {
		return fmt.Errorf("Chain already exists")
	}
	if !s.exists {
		s.b.AddTable(s.fam, s.table, "", false)
		s.exists = true
	}
	s.b.AddChain(&nft.ChainSpec{Family: s.fam, Table: s.table, Name: chain}, false)
	s.chains[chain] = true
	return nil
}

func (s *state) flush(chain string) error {
	if !s.has(chain) {
		return fmt.Errorf("No chain/target/match by that name")
	}
	if s.chains[chain] {
		s.b.FlushChain(s.fam, s.table, chain)
	}
	s.rules[chain] = nil
	return nil
}

func (s *state) deleteChain(chain string) error {
	if s.isBuiltin(chain) {
		return fmt.Errorf("Can't delete built-in chain")
	}
	if !s.chains[chain] {
		return fmt.Errorf("No chain/target/match by that name")
	}
	s.b.DelChain(s.fam, s.table, chain)
	delete(s.chains, chain)
	return nil
}

func (s *state) find(chain string, r *rule) *nft.RuleSpec {
	want := r.canonical()
	for i := range s.rules[chain] {
		if s.rules[chain][i].Comment == want {
			return &s.rules[chain][i]
		}
	}
	return nil
}

func (s *state) appendRule(chain string, r *rule) error {
	if !s.has(chain) {
		return fmt.Errorf("No chain/target/match by that name")
	}
	text, err := r.nftText(s.fam, s.has)
	if err != nil {
		return err
	}
	spec, err := nft.CompileRuleText(s.fam, s.table, chain, text, nil)
	if err != nil {
		return err
	}
	spec.Comment = r.canonical()
	if err := s.ensure(chain); err != nil {
		return err
	}
	s.b.AddRule(spec)
	s.rules[chain] = append(s.rules[chain], *spec)
	return nil
}

func (s *state) deleteRule(chain string, r *rule) error {
	found := s.find(chain, r)
	if found == nil || found.Handle == 0 {
		return fmt.Errorf("Bad rule (does a matching rule exist in that chain?)")
	}
	s.b.DelRule(s.fam, s.table, chain, found.Handle)
	for i := range s.rules[chain] {
		if &s.rules[chain][i] == found {
			s.rules[chain] = append(s.rules[chain][:i], s.rules[chain][i+1:]...)
			break
		}
	}
	return nil
}

// stripWait removes -w/--wait [N] and -W/--wait-interval N: there is no
// xtables lock to wait for — every change is one netlink transaction.
func stripWait(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-w" || a == "--wait":
			if i+1 < len(args) && isNumber(args[i+1]) {
				i++
			}
		case strings.HasPrefix(a, "--wait="):
		case a == "-W" || a == "--wait-interval":
			i++
		default:
			out = append(out, a)
		}
	}
	return out
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func fail(rc *tool.RunContext, t *tool.Tool, err error) int {
	var ue usageErr
	if errors.As(err, &ue) {
		fmt.Fprintf(rc.Err, "%s v%s (nf_tables): %s\nTry `%s -h' or '%s --help' for more information.\n", t.Name, Version, ue.msg, t.Name, t.Name)
		return 2
	}
	fmt.Fprintf(rc.Err, "%s: %v.\n", t.Name, err)
	return 1
}

func runIptables(rc *tool.RunContext, t *tool.Tool, fam nft.Family, args []string) int {
	args = stripWait(args)
	table := "filter"
	var op, chain string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--version", "-V":
			fmt.Fprintf(rc.Out, "%s v%s (nf_tables)\n", t.Name, Version)
			return 0
		case "-h", "--help":
			fmt.Fprintf(rc.Out, "Usage: %s\n%s\n", t.Usage, t.Synopsis)
			return 0
		case "-t", "--table":
			if i+1 >= len(args) {
				return fail(rc, t, usageErr{"option \"-t\" requires an argument"})
			}
			i++
			table = args[i]
		case "-A", "-C", "-D", "-N", "-F", "-X", "-S",
			"--append", "--check", "--delete", "--new-chain", "--flush", "--delete-chain", "--list-rules":
			if op != "" {
				return fail(rc, t, usageErr{"multiple commands"})
			}
			op = map[string]string{"--append": "-A", "--check": "-C", "--delete": "-D", "--new-chain": "-N",
				"--flush": "-F", "--delete-chain": "-X", "--list-rules": "-S"}[a]
			if op == "" {
				op = a
			}
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && args[i+1] != "!" {
				i++
				chain = args[i]
			}
		default:
			if op == "" {
				return fail(rc, t, unsupported("option %q", a))
			}
			rest = append(rest, a)
		}
	}
	if op == "" {
		return fail(rc, t, usageErr{"no command specified"})
	}
	b, err := newBackend()
	if err != nil {
		return fail(rc, t, err)
	}
	s, err := load(b, fam, table)
	if err != nil {
		return fail(rc, t, err)
	}

	needChain := op == "-A" || op == "-C" || op == "-D" || op == "-N"
	if needChain && chain == "" {
		return fail(rc, t, usageErr{fmt.Sprintf("option %q requires a chain", op)})
	}
	switch op {
	case "-S":
		return listRules(rc, t, s, chain, rest)
	case "-N", "-X":
		if len(rest) > 0 {
			return fail(rc, t, usageErr{"unexpected " + rest[0]})
		}
		if op == "-N" {
			err = s.newChain(chain)
		} else if chain == "" {
			return fail(rc, t, unsupported("-X without a chain"))
		} else {
			err = s.deleteChain(chain)
		}
	case "-F":
		if len(rest) > 0 {
			return fail(rc, t, usageErr{"unexpected " + rest[0]})
		}
		if chain == "" {
			for c := range s.chains {
				_ = s.flush(c)
			}
		} else {
			err = s.flush(chain)
		}
	case "-A", "-C", "-D":
		r, perr := parseRule(rest, fam)
		if perr != nil {
			return fail(rc, t, perr)
		}
		switch op {
		case "-C":
			if s.find(chain, r) == nil {
				return fail(rc, t, fmt.Errorf("Bad rule (does a matching rule exist in that chain?)"))
			}
			return 0
		case "-A":
			err = s.appendRule(chain, r)
		case "-D":
			err = s.deleteRule(chain, r)
		}
	}
	if err != nil {
		return fail(rc, t, err)
	}
	if err := b.Commit(); err != nil {
		return fail(rc, t, err)
	}
	return 0
}

func listRules(rc *tool.RunContext, t *tool.Tool, s *state, chain string, rest []string) int {
	if len(rest) > 1 || (len(rest) == 1 && (chain == "" || !isNumber(rest[0]))) {
		return fail(rc, t, unsupported("-S %s", strings.Join(rest, " ")))
	}
	line := func(c string) {
		if s.isBuiltin(c) {
			fmt.Fprintf(rc.Out, "-P %s ACCEPT\n", c)
		} else {
			fmt.Fprintf(rc.Out, "-N %s\n", c)
		}
	}
	if chain != "" {
		if !s.has(chain) {
			return fail(rc, t, fmt.Errorf("No chain/target/match by that name"))
		}
		if len(rest) == 1 {
			n, _ := strconv.Atoi(rest[0])
			if n == 0 {
				line(chain)
			} else if n <= len(s.rules[chain]) {
				fmt.Fprintf(rc.Out, "-A %s %s\n", chain, s.rules[chain][n-1].Comment)
			}
			return 0
		}
		line(chain)
		for _, r := range s.rules[chain] {
			fmt.Fprintf(rc.Out, "-A %s %s\n", chain, r.Comment)
		}
		return 0
	}
	var names []string
	for _, c := range []string{"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"} {
		if s.isBuiltin(c) {
			names = append(names, c)
		}
	}
	for c := range s.chains {
		if !s.isBuiltin(c) {
			names = append(names, c)
		}
	}
	sortUser(names, s)
	for _, c := range names {
		line(c)
	}
	for _, c := range names {
		for _, r := range s.rules[c] {
			fmt.Fprintf(rc.Out, "-A %s %s\n", c, r.Comment)
		}
	}
	return 0
}

// sortUser keeps built-ins first (in hook order) and sorts user chains.
func sortUser(names []string, s *state) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && !s.isBuiltin(names[j]) && !s.isBuiltin(names[j-1]) && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

// splitLine splits an iptables-restore line on blanks, honoring "…" quotes.
func splitLine(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQ && c == '\\' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\\'):
			cur.WriteByte(line[i+1])
			i++
		case c == '"':
			inQ, have = !inQ, true
		case !inQ && (c == ' ' || c == '\t'):
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if inQ {
		return nil, fmt.Errorf("unterminated quote")
	}
	if have {
		out = append(out, cur.String())
	}
	return out, nil
}

func runRestore(rc *tool.RunContext, t *tool.Tool, fam nft.Family, args []string) int {
	args = stripWait(args)
	noflush := false
	only := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--version", "-V":
			fmt.Fprintf(rc.Out, "%s v%s (nf_tables)\n", t.Name, Version)
			return 0
		case "-n", "--noflush":
			noflush = true
		case "-c", "--counters":
		case "-T", "--table":
			if i+1 >= len(args) {
				return fail(rc, t, usageErr{"option \"-T\" requires an argument"})
			}
			i++
			only = args[i]
		default:
			return fail(rc, t, unsupported("option %q", a))
		}
	}
	if !noflush {
		return fail(rc, t, unsupported("restore without --noflush"))
	}
	data, err := io.ReadAll(rc.In)
	if err != nil {
		return fail(rc, t, err)
	}
	b, err := newBackend()
	if err != nil {
		return fail(rc, t, err)
	}
	var s *state
	for n, line := range strings.Split(string(data), "\n") {
		lineErr := func(err error) int {
			var ue usageErr
			if errors.As(err, &ue) {
				return fail(rc, t, usageErr{fmt.Sprintf("line %d: %s", n+1, ue.msg)})
			}
			fmt.Fprintf(rc.Err, "%s: line %d failed: %v\n", t.Name, n+1, err)
			return 1
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "*"):
			if s, err = load(b, fam, line[1:]); err != nil {
				return lineErr(err)
			}
			continue
		case line == "COMMIT":
			s = nil
			continue
		}
		if s == nil {
			return lineErr(usageErr{"no table specified"})
		}
		if only != "" && s.table != only {
			continue
		}
		f, err := splitLine(line)
		if err != nil {
			return lineErr(usageErr{err.Error()})
		}
		if strings.HasPrefix(f[0], ":") {
			// ":CHAIN POLICY [p:b]" declares (and, for a user chain, flushes) it.
			c := f[0][1:]
			switch {
			case s.isBuiltin(c):
				err = s.ensure(c)
			case s.chains[c]:
				err = s.flush(c)
			default:
				err = s.newChain(c)
			}
		} else if len(f) < 2 {
			err = usageErr{"bad line"}
		} else {
			switch f[0] {
			case "-A", "-D":
				r, perr := parseRule(f[2:], fam)
				if perr != nil {
					return lineErr(perr)
				}
				if f[0] == "-A" {
					err = s.appendRule(f[1], r)
				} else {
					err = s.deleteRule(f[1], r)
				}
			case "-X":
				err = s.deleteChain(f[1])
			case "-F":
				err = s.flush(f[1])
			case "-N":
				err = s.newChain(f[1])
			default:
				err = unsupported("restore command %q", f[0])
			}
		}
		if err != nil {
			return lineErr(err)
		}
	}
	if s != nil {
		return fail(rc, t, usageErr{"missing COMMIT"})
	}
	if err := b.Commit(); err != nil {
		return fail(rc, t, err)
	}
	return 0
}
