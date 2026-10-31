package nft

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Registers, as the kernel numbers them: NFT_REG_VERDICT, the 16-byte
// NFT_REG_1.., and the 4-byte NFT_REG32_00.. (8..) that concatenations use.
const (
	regVerdict = 0
	reg1       = 1
	reg2       = 2
	reg32Base  = 8
)

// regAt is the register holding the word at byte offset off of a value
// loaded at reg1: nft spells word 0 "reg 1" and word n "reg 8+n".
func regAt(off int) uint32 {
	if off == 0 {
		return reg1
	}
	return uint32(reg32Base + off/4)
}

// SetLookup resolves a named set of the rule's table (from the same
// transaction or the kernel).
type SetLookup func(name string) (*SetSpec, error)

// selector is a value source in a rule: a payload field, meta/ct key, …
type selector struct {
	name    string
	typ     DataType
	load    func(dreg uint32) []Expr
	bitmask bool // ct state/status: "ct state new" means (x & new) != 0
	values  map[string]uint32
}

type compiler struct {
	fam  Family
	t    []token
	pos  int
	r    *RuleSpec
	sets SetLookup
}

// CompileRuleText compiles rule text (everything after the chain name).
func CompileRuleText(fam Family, table, chain, text string, sets SetLookup) (*RuleSpec, error) {
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	return CompileRule(fam, table, chain, toks, sets)
}

// CompileRule compiles the text of one rule (everything after the chain name).
func CompileRule(fam Family, table, chain string, toks []token, sets SetLookup) (*RuleSpec, error) {
	c := &compiler{fam: fam, t: toks, r: &RuleSpec{Family: fam, Table: table, Chain: chain}, sets: sets}
	for !c.eof() {
		if err := c.statement(); err != nil {
			return nil, err
		}
	}
	return c.r, nil
}

func (c *compiler) eof() bool   { return c.pos >= len(c.t) }
func (c *compiler) peek() token { return c.at(0) }
func (c *compiler) at(n int) token {
	if c.pos+n >= len(c.t) {
		return token{kind: tEOL}
	}
	return c.t[c.pos+n]
}
func (c *compiler) next() token    { t := c.peek(); c.pos++; return t }
func (c *compiler) emit(e ...Expr) { c.r.Exprs = append(c.r.Exprs, e...) }

func (c *compiler) expect(words ...string) error {
	for _, w := range words {
		if t := c.next(); !t.is(w) {
			return fmt.Errorf("syntax error: expected %q, got %s", w, t)
		}
	}
	return nil
}

func (c *compiler) word() (string, error) {
	t := c.next()
	if t.kind != tWord && t.kind != tString {
		return "", fmt.Errorf("syntax error: unexpected %s", t)
	}
	return t.val, nil
}

func (c *compiler) statement() error {
	t := c.peek()
	switch {
	case t.is("accept"), t.is("drop"), t.is("return"), t.is("continue"):
		c.next()
		c.emit(VerdictExpr{Verdict{Kind: t.val}})
		return nil
	case t.is("jump"), t.is("goto"):
		c.next()
		name, err := c.word()
		if err != nil {
			return err
		}
		c.emit(VerdictExpr{Verdict{Kind: t.val, Chain: name}})
		return nil
	case t.is("comment"):
		c.next()
		s := c.next()
		if s.kind != tString && s.kind != tWord {
			return fmt.Errorf("syntax error: comment needs a string")
		}
		c.r.Comment = s.val
		return nil
	case t.is("counter"):
		c.next()
		if c.peek().is("name") {
			c.next()
			name, err := c.word()
			if err != nil {
				return err
			}
			c.emit(ObjRef{Name: name})
			return nil
		}
		c.emit(Counter{})
		return nil
	case t.is("reject"):
		c.next()
		if c.peek().is("with") {
			return fmt.Errorf("unsupported: reject with")
		}
		code := uint8(3) // ICMP port unreachable
		if c.fam == FamilyIP6 {
			code = 4 // ICMPv6 port unreachable
		}
		c.emit(Reject{Type: 0, Code: code})
		return nil
	case t.is("masquerade"):
		c.next()
		m := Masq{}
		for {
			switch {
			case c.peek().is("fully-random"):
				m.FullyRandom = true
			case c.peek().is("random"):
				m.Random = true
			case c.peek().is("persistent"):
				m.Persistent = true
			case c.peek().is(","):
			default:
				c.emit(m)
				return nil
			}
			c.next()
		}
	case t.is("dnat"), t.is("snat"):
		return c.nat()
	case t.is("update"), t.is("add") && c.at(1).is("@"):
		return c.dynset()
	case (t.is("mark") || ((t.is("meta") || t.is("ct")) && c.at(1).is("mark"))) && (c.at(1).is("set") || c.at(2).is("set")):
		return c.markSet()
	}
	return c.match()
}

// selectorAt parses one selector at the cursor, or returns nil.
func (c *compiler) selector() (*selector, error) {
	t := c.peek()
	switch {
	case t.is("ip"), t.is("ip6"):
		fam := Family(t.val)
		field := c.at(1)
		if !field.is("saddr") && !field.is("daddr") {
			return nil, nil
		}
		if fam != c.fam && c.fam != FamilyInet {
			return nil, fmt.Errorf("%s %s is not valid in a %s table", t.val, field.val, c.fam)
		}
		c.pos += 2
		off, n, dt := uint32(12), uint32(4), dataTypes["ipv4_addr"]
		if fam == FamilyIP6 {
			off, n, dt = 8, 16, dataTypes["ipv6_addr"]
		}
		if field.val == "daddr" {
			off += n
		}
		return &selector{name: t.val + " " + field.val, typ: dt, load: func(r uint32) []Expr {
			return []Expr{Payload{Base: BaseNetwork, Offset: off, Len: n, DReg: r}}
		}}, nil
	case t.is("th"), t.is("tcp"), t.is("udp"), t.is("sctp"):
		field := c.at(1)
		if !field.is("dport") && !field.is("sport") {
			return nil, fmt.Errorf("unsupported: %s %s", t.val, field)
		}
		c.pos += 2
		off := uint32(0)
		if field.val == "dport" {
			off = 2
		}
		proto := t.val
		return &selector{name: proto + " " + field.val, typ: dataTypes["inet_service"], load: func(r uint32) []Expr {
			var ex []Expr
			if proto != "th" {
				ex = append(ex, Meta{Key: "l4proto", DReg: r}, Cmp{Op: "eq", Reg: r, Data: []byte{protoNumbers[proto]}})
			}
			return append(ex, Payload{Base: BaseTransport, Offset: off, Len: 2, DReg: r})
		}}, nil
	case t.is("meta"), t.is("mark"), t.is("iifname"), t.is("oifname"):
		key := t.val
		n := 1
		if key == "meta" {
			key = c.at(1).val
			n = 2
		}
		var dt DataType
		switch key {
		case "l4proto":
			dt = dataTypes["inet_proto"]
		case "mark":
			dt = dataTypes["mark"]
		case "iifname", "oifname":
			dt = dataTypes["ifname"]
		default:
			return nil, fmt.Errorf("unsupported: meta %s", key)
		}
		c.pos += n
		return &selector{name: "meta " + key, typ: dt, load: func(r uint32) []Expr {
			return []Expr{Meta{Key: key, DReg: r}}
		}}, nil
	case t.is("ct"):
		key := c.at(1).val
		var vals map[string]uint32
		switch key {
		case "state":
			vals = map[string]uint32{"invalid": 1, "established": 2, "related": 4, "new": 8, "untracked": 64}
		case "status":
			vals = map[string]uint32{"expected": 1, "seen-reply": 2, "assured": 4, "confirmed": 8, "snat": 16, "dnat": 32, "dying": 512}
		case "mark":
			c.pos += 2
			return &selector{name: "ct mark", typ: dataTypes["mark"], load: func(r uint32) []Expr { return []Expr{Ct{Key: "mark", DReg: r}} }}, nil
		default:
			return nil, fmt.Errorf("unsupported: ct %s", key)
		}
		c.pos += 2
		return &selector{name: "ct " + key, typ: DataType{"ct_" + key, 4}, bitmask: true, values: vals, load: func(r uint32) []Expr {
			return []Expr{Ct{Key: key, DReg: r}}
		}}, nil
	case t.is("fib"):
		addr := c.at(1)
		if (!addr.is("saddr") && !addr.is("daddr")) || !c.at(2).is("type") {
			return nil, fmt.Errorf("unsupported: fib %s %s", addr, c.at(2))
		}
		c.pos += 3
		vals := map[string]uint32{"unspec": 0, "unicast": 1, "local": 2, "broadcast": 3, "anycast": 4, "multicast": 5, "blackhole": 6, "unreachable": 7, "prohibit": 8}
		return &selector{name: "fib " + addr.val + " type", typ: DataType{"fib_addrtype", 4}, values: vals, load: func(r uint32) []Expr {
			return []Expr{Fib{SAddr: addr.val == "saddr", DAddr: addr.val == "daddr", DReg: r}}
		}}, nil
	case t.is("numgen"):
		if !c.at(1).is("random") || !c.at(2).is("mod") {
			return nil, fmt.Errorf("unsupported: numgen %s", c.at(1))
		}
		mod, err := strconv.ParseUint(c.at(3).val, 10, 32)
		if err != nil || mod == 0 {
			return nil, fmt.Errorf("bad numgen modulus %q", c.at(3).val)
		}
		c.pos += 4
		return &selector{name: "numgen", typ: dataTypes["integer"], load: func(r uint32) []Expr {
			return []Expr{Numgen{Mod: uint32(mod), DReg: r}}
		}}, nil
	}
	return nil, nil
}

// concat parses "sel [. sel]..." and emits the loads; returns the types.
func (c *compiler) concat() ([]*selector, error) {
	first, err := c.selector()
	if err != nil {
		return nil, err
	}
	if first == nil {
		return nil, fmt.Errorf("syntax error: unexpected %s", c.peek())
	}
	sels := []*selector{first}
	for c.peek().is(".") {
		c.next()
		s, err := c.selector()
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("syntax error: unexpected %s in concatenation", c.peek())
		}
		sels = append(sels, s)
	}
	if len(sels) == 1 {
		c.emit(first.load(reg1)...)
		return sels, nil
	}
	off := 0
	for _, s := range sels {
		if s.bitmask || s.values != nil {
			return nil, fmt.Errorf("unsupported: %s in a concatenation", s.name)
		}
		c.emit(s.load(regAt(off))...)
		off += padLen(s.typ.Len)
	}
	return sels, nil
}

func types(sels []*selector) []DataType {
	out := make([]DataType, len(sels))
	for i, s := range sels {
		out[i] = s.typ
	}
	return out
}

// match compiles "SELECTOR [. SELECTOR]... RELATION".
func (c *compiler) match() error {
	sels, err := c.concat()
	if err != nil {
		return err
	}
	keyTypes := types(sels)
	single := sels[0]

	// mark and MASK [!=|==] VALUE
	if c.peek().is("and") && len(sels) == 1 && (single.typ.Name == "mark" || single.typ.Name == "integer") {
		c.next()
		mask, _, err := encodeValue(single.typ, c.next().val)
		if err != nil {
			return err
		}
		op := "eq"
		if c.peek().is("!=") {
			op = "neq"
			c.next()
		} else if c.peek().is("==") {
			c.next()
		}
		val, _, err := encodeValue(single.typ, c.next().val)
		if err != nil {
			return err
		}
		c.emit(Bitwise{SReg: reg1, DReg: reg1, Len: 4, Mask: mask, Xor: make([]byte, 4)}, Cmp{Op: op, Reg: reg1, Data: val})
		return nil
	}

	neg := false
	if c.peek().is("!=") {
		neg = true
		c.next()
	} else if c.peek().is("==") {
		c.next()
	}

	switch {
	case c.peek().is("@"):
		c.next()
		name, err := c.word()
		if err != nil {
			return err
		}
		if err := c.checkSet(name, keyTypes, false); err != nil {
			return err
		}
		c.emit(Lookup{SReg: reg1, Set: name, Invert: neg})
		return nil
	case c.peek().is("{"):
		return c.anonSet(single, keyTypes, neg)
	case c.peek().is("vmap"):
		if neg {
			return fmt.Errorf("syntax error: != vmap")
		}
		c.next()
		if c.peek().is("@") {
			c.next()
			name, err := c.word()
			if err != nil {
				return err
			}
			if err := c.checkSet(name, keyTypes, true); err != nil {
				return err
			}
			c.emit(Lookup{SReg: reg1, DReg: regVerdict, HasDReg: true, Set: name})
			return nil
		}
		return c.anonVmap(keyTypes)
	}

	if len(sels) > 1 {
		return fmt.Errorf("unsupported: concatenation compared with a literal")
	}
	v := c.next()
	if v.kind == tEOL || v.kind == tPunct {
		return fmt.Errorf("syntax error: %s needs a value", single.name)
	}
	if single.values != nil {
		var bits uint32
		for _, name := range strings.Split(v.val, ",") {
			b, ok := single.values[name]
			if !ok {
				return fmt.Errorf("bad %s value %q", single.name, name)
			}
			bits |= b
		}
		// Comma lists like "ct state established,related" arrive as one word
		// or as word , word.
		for c.peek().is(",") && single.bitmask {
			c.next()
			b, ok := single.values[c.peek().val]
			if !ok {
				return fmt.Errorf("bad %s value %q", single.name, c.peek().val)
			}
			c.next()
			bits |= b
		}
		val := binary.NativeEndian.AppendUint32(nil, bits)
		if single.bitmask {
			op := "neq"
			if neg {
				op = "eq"
			}
			c.emit(Bitwise{SReg: reg1, DReg: reg1, Len: 4, Mask: val, Xor: make([]byte, 4)}, Cmp{Op: op, Reg: reg1, Data: make([]byte, 4)})
			return nil
		}
		c.emit(Cmp{Op: opName(neg), Reg: reg1, Data: val})
		return nil
	}
	if single.typ.Name == "ifname" && strings.HasSuffix(v.val, "*") {
		prefix := strings.TrimSuffix(v.val, "*")
		c.emit(Cmp{Op: opName(neg), Reg: reg1, Data: []byte(prefix)})
		return nil
	}
	return c.compareLiteral(single, v.val, neg)
}

// compareLiteral emits "x == v" / "x != v" for a plain or prefix literal
// already loaded in reg 1.
func (c *compiler) compareLiteral(single *selector, val string, neg bool) error {
	b, plen, err := encodeValue(single.typ, val)
	if err != nil {
		return err
	}
	if plen >= 0 {
		if plen == 0 {
			if neg {
				return fmt.Errorf("unsupported: != %s", val)
			}
			return nil // x/0 matches everything
		}
		c.emit(Bitwise{SReg: reg1, DReg: reg1, Len: uint32(len(b)), Mask: prefixMask(len(b), plen), Xor: make([]byte, len(b))})
	}
	c.emit(Cmp{Op: opName(neg), Reg: reg1, Data: b})
	return nil
}

func opName(neg bool) string {
	if neg {
		return "neq"
	}
	return "eq"
}

func (c *compiler) checkSet(name string, keys []DataType, wantVmap bool) error {
	if c.sets == nil {
		return nil
	}
	s, err := c.sets(name)
	if err != nil {
		return err
	}
	if keyLen(s.Keys) != keyLen(keys) {
		return fmt.Errorf("datatype mismatch: @%s key does not match the expression", name)
	}
	if wantVmap && !s.IsVerdictMap() {
		return fmt.Errorf("@%s is not a verdict map", name)
	}
	if !wantVmap && s.IsMap() {
		return fmt.Errorf("@%s is a map, not a set", name)
	}
	return nil
}

// braceToks consumes "{ ... }" and returns the inner tokens.
func (c *compiler) braceToks() ([]token, error) {
	if !c.peek().is("{") {
		return nil, fmt.Errorf("syntax error: expected {")
	}
	start := c.pos + 1
	depth := 0
	for !c.eof() {
		t := c.next()
		if t.is("{") {
			depth++
		} else if t.is("}") {
			depth--
			if depth == 0 {
				return c.t[start : c.pos-1], nil
			}
		}
	}
	return nil, fmt.Errorf("syntax error: unbalanced {")
}

func (c *compiler) anonSet(single *selector, keyTypes []DataType, neg bool) error {
	inner, err := c.braceToks()
	if err != nil {
		return err
	}
	var parts [][]token
	for _, part := range splitTop(inner, ",") {
		if len(part) > 0 {
			parts = append(parts, part)
		}
	}
	// A one-element set is a plain comparison, as nft compiles it.
	if len(parts) == 1 && len(keyTypes) == 1 && len(parts[0]) == 1 && single.values == nil && single.typ.Name != "ifname" {
		return c.compareLiteral(single, parts[0][0].val, neg)
	}
	spec := SetSpec{Family: c.fam, Table: c.r.Table, Keys: keyTypes, Anonymous: true, Constant: true}
	var elems []Element
	var ranges [][2][]byte
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		fields := splitTop(part, ".")
		if len(keyTypes) == 1 && len(fields) == 1 && len(fields[0]) == 1 && (keyTypes[0].Name == "ipv4_addr" || keyTypes[0].Name == "ipv6_addr") {
			b, plen, err := encodeValue(keyTypes[0], fields[0][0].val)
			if err != nil {
				return err
			}
			if plen < 0 {
				plen = len(b) * 8
			}
			lo, hi := prefixRange(b, plen)
			ranges = append(ranges, [2][]byte{lo, hi})
			continue
		}
		key, err := encodeConcat(keyTypes, fields)
		if err != nil {
			return err
		}
		elems = append(elems, Element{Key: key})
	}
	if ranges != nil {
		interval := false
		for _, r := range ranges {
			if string(r[0]) != string(r[1]) {
				interval = true
			}
		}
		if interval {
			spec.Interval = true
			elems = intervalElements(ranges)
		} else {
			for _, r := range ranges {
				elems = append(elems, Element{Key: r[0]})
			}
		}
	}
	if len(elems) == 0 {
		return fmt.Errorf("syntax error: empty set")
	}
	c.r.AnonSets = append(c.r.AnonSets, &AnonSet{Spec: spec, Elements: elems})
	c.emit(Lookup{SReg: reg1, Anon: len(c.r.AnonSets) - 1, Invert: neg})
	return nil
}

func (c *compiler) anonVmap(keyTypes []DataType) error {
	inner, err := c.braceToks()
	if err != nil {
		return err
	}
	spec := SetSpec{Family: c.fam, Table: c.r.Table, Keys: keyTypes, Data: []DataType{dataTypes["verdict"]}, Anonymous: true, Constant: true}
	var elems []Element
	for _, part := range splitTop(inner, ",") {
		if len(part) == 0 {
			continue
		}
		e, err := parseRawElement(part)
		if err != nil {
			return err
		}
		if e.Value == nil || len(e.Value) != 1 {
			return fmt.Errorf("syntax error: vmap element needs a verdict")
		}
		key, err := encodeConcat(keyTypes, e.Key)
		if err != nil {
			return err
		}
		v, err := parseVerdict(e.Value[0])
		if err != nil {
			return err
		}
		elems = append(elems, Element{Key: key, Verdict: v})
	}
	if len(elems) == 0 {
		return fmt.Errorf("syntax error: empty vmap")
	}
	c.r.AnonSets = append(c.r.AnonSets, &AnonSet{Spec: spec, Elements: elems})
	c.emit(Lookup{SReg: reg1, DReg: regVerdict, HasDReg: true, Anon: len(c.r.AnonSets) - 1})
	return nil
}

func (c *compiler) addrType() DataType {
	if c.fam == FamilyIP6 {
		return dataTypes["ipv6_addr"]
	}
	return dataTypes["ipv4_addr"]
}

// nat compiles:
//
//	dnat|snat to ADDR[:PORT]           (ip6: [ADDR]:PORT)
//	dnat|snat [ip|ip6] to ADDR
//	dnat|snat ip|ip6 addr . port to numgen random mod N map { K : ADDR . PORT, ... }
func (c *compiler) nat() error {
	typ := c.next().val
	natFam := c.fam
	if c.peek().is("ip") || c.peek().is("ip6") {
		if Family(c.peek().val) != c.fam && c.fam != FamilyInet {
			return fmt.Errorf("%s %s is not valid in a %s table", typ, c.peek().val, c.fam)
		}
		natFam = Family(c.peek().val)
		c.next()
	}
	at := c.addrType()
	if natFam == FamilyIP6 {
		at = dataTypes["ipv6_addr"]
	}
	if c.peek().is("addr") {
		if err := c.expect("addr", ".", "port", "to"); err != nil {
			return err
		}
		sels, err := c.concat()
		if err != nil {
			return err
		}
		if !c.peek().is("map") {
			return fmt.Errorf("syntax error: expected map after %s", sels[0].name)
		}
		c.next()
		inner, err := c.braceToks()
		if err != nil {
			return err
		}
		dataTypesList := []DataType{at, dataTypes["inet_service"]}
		spec := SetSpec{Family: c.fam, Table: c.r.Table, Keys: types(sels), Data: dataTypesList, Anonymous: true, Constant: true}
		var elems []Element
		for _, part := range splitTop(inner, ",") {
			if len(part) == 0 {
				continue
			}
			e, err := parseRawElement(part)
			if err != nil {
				return err
			}
			if e.Value == nil {
				return fmt.Errorf("syntax error: map element needs a value")
			}
			key, err := encodeConcat(spec.Keys, e.Key)
			if err != nil {
				return err
			}
			data, err := encodeConcat(dataTypesList, e.Value)
			if err != nil {
				return err
			}
			elems = append(elems, Element{Key: key, Data: data})
		}
		if len(elems) == 0 {
			return fmt.Errorf("syntax error: empty map")
		}
		c.r.AnonSets = append(c.r.AnonSets, &AnonSet{Spec: spec, Elements: elems})
		c.emit(Lookup{SReg: reg1, DReg: reg1, HasDReg: true, Anon: len(c.r.AnonSets) - 1},
			Nat{Type: typ, Family: natFam, RegAddr: reg1, RegProto: regAt(padLen(at.Len))})
		return nil
	}
	if err := c.expect("to"); err != nil {
		return err
	}
	addr, port, err := splitHostPort(c.next().val, natFam)
	if err != nil {
		return err
	}
	ab, plen, err := encodeValue(at, addr)
	if err != nil || plen >= 0 {
		return fmt.Errorf("bad %s address %q", typ, addr)
	}
	c.emit(Immediate{DReg: reg1, Data: ab})
	n := Nat{Type: typ, Family: natFam, RegAddr: reg1}
	if port != "" {
		pb, _, err := encodeValue(dataTypes["inet_service"], port)
		if err != nil {
			return err
		}
		c.emit(Immediate{DReg: reg2, Data: pb})
		n.RegProto = reg2
	}
	c.emit(n)
	return nil
}

// splitHostPort splits "1.2.3.4:80", "[fd00::1]:80", "1.2.3.4" or "fd00::1".
func splitHostPort(s string, fam Family) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		i := strings.Index(s, "]")
		if i < 0 {
			return "", "", fmt.Errorf("bad address %q", s)
		}
		host, rest := s[1:i], s[i+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("bad address %q", s)
		}
		return host, rest[1:], nil
	}
	if fam == FamilyIP6 {
		return s, "", nil
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:], nil
	}
	return s, "", nil
}

// dynset compiles "update|add @SET { SELECTOR [. SELECTOR]... }".
func (c *compiler) dynset() error {
	op := c.next().val
	if err := c.expect("@"); err != nil {
		return err
	}
	name, err := c.word()
	if err != nil {
		return err
	}
	if err := c.expect("{"); err != nil {
		return err
	}
	sels, err := c.concat()
	if err != nil {
		return err
	}
	if err := c.expect("}"); err != nil {
		return err
	}
	if err := c.checkSet(name, types(sels), false); err != nil {
		return err
	}
	c.emit(Dynset{Op: op, SReg: reg1, Set: name})
	return nil
}

// markSet compiles "[meta] mark set mark (or|xor|and) V" and "[meta] mark set V".
func (c *compiler) markSet() error {
	dest := "meta"
	if c.peek().is("meta") || c.peek().is("ct") {
		dest = c.next().val
	}
	if err := c.expect("mark", "set"); err != nil {
		return err
	}
	mt := dataTypes["mark"]
	if c.peek().is("mark") || ((c.peek().is("meta") || c.peek().is("ct")) && c.at(1).is("mark")) {
		source := "meta"
		if c.peek().is("meta") || c.peek().is("ct") {
			source = c.next().val
		}
		c.next() // mark
		op := c.next().val
		v, _, err := encodeValue(mt, c.next().val)
		if err != nil {
			return err
		}
		val := binary.NativeEndian.Uint32(v)
		var mask, xor uint32
		switch op {
		case "or":
			mask, xor = ^val, val
		case "xor":
			mask, xor = 0xffffffff, val
		case "and":
			mask, xor = val, 0
		default:
			return fmt.Errorf("unsupported: mark set mark %s", op)
		}
		if source == "ct" {
			c.emit(Ct{Key: "mark", DReg: reg1})
		} else {
			c.emit(Meta{Key: "mark", DReg: reg1})
		}
		c.emit(Bitwise{SReg: reg1, DReg: reg1, Len: 4, Mask: binary.NativeEndian.AppendUint32(nil, mask), Xor: binary.NativeEndian.AppendUint32(nil, xor)})
		if dest == "ct" {
			c.emit(Ct{Key: "mark", Set: true, SReg: reg1})
		} else {
			c.emit(Meta{Key: "mark", Set: true, SReg: reg1})
		}
		return nil
	}
	v, _, err := encodeValue(mt, c.next().val)
	if err != nil {
		return err
	}
	c.emit(Immediate{DReg: reg1, Data: v})
	if dest == "ct" {
		c.emit(Ct{Key: "mark", Set: true, SReg: reg1})
	} else {
		c.emit(Meta{Key: "mark", Set: true, SReg: reg1})
	}
	return nil
}
