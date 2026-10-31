//go:build linux

package nft

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// netlinkBackend drives the kernel through github.com/google/nftables. All
// writes are queued on one Conn and sent as a single batch by Commit.
type netlinkBackend struct {
	conn *nftables.Conn
	err  error
}

// NewNetlink returns the kernel backend.
func NewNetlink() (Backend, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	return &netlinkBackend{conn: c}, nil
}

var nlFamilies = map[Family]nftables.TableFamily{
	FamilyIP: nftables.TableFamilyIPv4, FamilyIP6: nftables.TableFamilyIPv6, FamilyInet: nftables.TableFamilyINet,
}

var nlTypes = map[string]nftables.SetDatatype{
	"ipv4_addr": nftables.TypeIPAddr, "ipv6_addr": nftables.TypeIP6Addr, "inet_proto": nftables.TypeInetProto,
	"inet_service": nftables.TypeInetService, "mark": nftables.TypeMark, "integer": nftables.TypeInteger,
	"ifname": nftables.TypeIFName, "verdict": nftables.TypeVerdict,
}

// nft datatype magics (6 bits per concatenated field) for decoding dumps.
var typeByMagic = map[uint32]DataType{
	4: dataTypes["integer"], 7: dataTypes["ipv4_addr"], 8: dataTypes["ipv6_addr"], 12: dataTypes["inet_proto"],
	13: dataTypes["inet_service"], 19: dataTypes["mark"], 41: dataTypes["ifname"],
}

const verdictMagic = 0xffffff00 // NFT_DATA_VERDICT

func nlTable(fam Family, name string) *nftables.Table {
	return &nftables.Table{Name: name, Family: nlFamilies[fam]}
}

func nlType(ts []DataType) nftables.SetDatatype {
	if len(ts) == 1 {
		return nlTypes[ts[0].Name]
	}
	parts := make([]nftables.SetDatatype, len(ts))
	for i, t := range ts {
		parts[i] = nlTypes[t.Name]
	}
	return nftables.MustConcatSetType(parts...)
}

func nlSet(s *SetSpec) *nftables.Set {
	ns := &nftables.Set{
		Table: nlTable(s.Family, s.Table), Name: s.Name, Anonymous: s.Anonymous, Constant: s.Constant,
		Interval: s.Interval, IsMap: s.IsMap(), HasTimeout: s.HasTimeout, Timeout: s.Timeout, Dynamic: s.Dynamic,
		KeyType: nlType(s.Keys), Comment: s.Comment,
	}
	if s.IsMap() {
		ns.DataType = nlType(s.Data)
	}
	return ns
}

var verdictKinds = map[string]expr.VerdictKind{
	"accept": expr.VerdictAccept, "drop": expr.VerdictDrop, "return": expr.VerdictReturn,
	"continue": expr.VerdictContinue, "jump": expr.VerdictJump, "goto": expr.VerdictGoto,
}

func nlVerdict(v *Verdict) *expr.Verdict {
	return &expr.Verdict{Kind: verdictKinds[v.Kind], Chain: v.Chain}
}

func nlElements(els []Element) []nftables.SetElement {
	out := make([]nftables.SetElement, len(els))
	for i, e := range els {
		out[i] = nftables.SetElement{Key: e.Key, IntervalEnd: e.IntervalEnd, Val: e.Data, Comment: e.Comment}
		if e.Verdict != nil {
			out[i].VerdictData = nlVerdict(e.Verdict)
		}
	}
	return out
}

func (b *netlinkBackend) GetTable(fam Family, name string) (*TableState, error) {
	t, err := b.conn.ListTableOfFamily(name, nlFamilies[fam])
	if err != nil {
		return nil, err
	}
	st := &TableState{Family: fam, Name: t.Name}
	chains, err := b.conn.ListChainsOfTableFamily(nlFamilies[fam])
	if err != nil {
		return nil, err
	}
	hooks := map[nftables.ChainHook]string{0: "prerouting", 1: "input", 2: "forward", 3: "output", 4: "postrouting"}
	for _, c := range chains {
		if c.Table == nil || c.Table.Name != name {
			continue
		}
		cs := ChainSpec{Family: fam, Table: name, Name: c.Name, Type: string(c.Type)}
		if c.Hooknum != nil {
			cs.Hook = hooks[*c.Hooknum]
		}
		if c.Priority != nil {
			cs.Priority = int32(*c.Priority)
		}
		if c.Policy != nil && *c.Policy == nftables.ChainPolicyDrop {
			cs.Policy = "drop"
		}
		st.Chains = append(st.Chains, cs)
		rules, err := b.conn.GetRules(t, c)
		if err != nil {
			return nil, err
		}
		for _, r := range rules {
			comment, _ := userdata.GetString(r.UserData, userdata.TypeComment)
			st.Rules = append(st.Rules, RuleSpec{Family: fam, Table: name, Chain: c.Name, Handle: r.Handle, Comment: comment, JSONExpr: readRuleJSON(r.UserData)})
		}
	}
	sets, err := dumpSets(fam, name, "")
	if err != nil {
		return nil, err
	}
	for _, s := range sets {
		if !s.Anonymous {
			st.Sets = append(st.Sets, *s)
		}
	}
	if st.Counters, err = b.GetCounters(fam, name); err != nil {
		return nil, err
	}
	return st, nil
}

func (b *netlinkBackend) GetSet(fam Family, table, name string) (*SetSpec, error) {
	sets, err := dumpSets(fam, table, name)
	if err != nil {
		return nil, err
	}
	if len(sets) != 1 {
		return nil, notFound("set %s", name)
	}
	return sets[0], nil
}

func (b *netlinkBackend) GetElements(s *SetSpec) ([]Element, error) {
	els, err := b.conn.GetSetElements(nlSet(s))
	if err != nil {
		return nil, err
	}
	out := make([]Element, len(els))
	for i, e := range els {
		out[i] = Element{Key: e.Key, IntervalEnd: e.IntervalEnd, Comment: e.Comment}
		if s.IsVerdictMap() {
			out[i].Verdict = decodeVerdict(e.Val)
		} else if s.IsMap() {
			out[i].Data = e.Val
		}
	}
	return out, nil
}

// decodeVerdict parses NFTA_VERDICT_CODE / NFTA_VERDICT_CHAIN attributes.
func decodeVerdict(b []byte) *Verdict {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return nil
	}
	ad.ByteOrder = binary.BigEndian
	v := &Verdict{}
	for ad.Next() {
		switch ad.Type() {
		case unix.NFTA_VERDICT_CODE:
			code := int32(ad.Uint32())
			for name, k := range verdictKinds {
				if int32(k) == code {
					v.Kind = name
				}
			}
		case unix.NFTA_VERDICT_CHAIN:
			v.Chain = ad.String()
		}
	}
	if v.Kind == "" {
		return nil
	}
	return v
}

func (b *netlinkBackend) GetCounters(fam Family, table string) ([]CounterSpec, error) {
	objs, err := b.conn.GetObjects(nlTable(fam, table))
	if err != nil {
		return nil, err
	}
	var out []CounterSpec
	for _, o := range objs {
		switch c := o.(type) {
		case *nftables.CounterObj:
			out = append(out, CounterSpec{Family: fam, Table: table, Name: c.Name, Packets: c.Packets, Bytes: c.Bytes})
		case *nftables.NamedObj:
			if ce, ok := c.Obj.(*expr.Counter); ok {
				out = append(out, CounterSpec{Family: fam, Table: table, Name: c.Name, Packets: ce.Packets, Bytes: ce.Bytes})
			}
		}
	}
	return out, nil
}

// dumpSets issues NFT_MSG_GETSET itself: google/nftables v0.3.0 overwrites
// a map's key type with the verdict data type when decoding, and kube-proxy
// needs the key types of existing verdict maps to encode their elements.
func dumpSets(fam Family, table, name string) ([]*SetSpec, error) {
	c, err := netlink.Dial(unix.NETLINK_NETFILTER, nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	attrs := []netlink.Attribute{{Type: unix.NFTA_SET_TABLE, Data: []byte(table + "\x00")}}
	flags := netlink.Request | netlink.Dump
	if name != "" {
		attrs = append(attrs, netlink.Attribute{Type: unix.NFTA_SET_NAME, Data: []byte(name + "\x00")})
		flags = netlink.Request
	}
	data, err := netlink.MarshalAttributes(attrs)
	if err != nil {
		return nil, err
	}
	hdr := []byte{byte(nlFamilies[fam]), unix.NFNETLINK_V0, 0, 0}
	msgs, err := c.Execute(netlink.Message{
		Header: netlink.Header{Type: netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_GETSET), Flags: flags},
		Data:   append(hdr, data...),
	})
	if err != nil {
		return nil, err
	}
	var out []*SetSpec
	for _, m := range msgs {
		if len(m.Data) < 4 {
			continue
		}
		s, err := decodeSet(fam, table, m.Data[4:])
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeSet(fam Family, table string, b []byte) (*SetSpec, error) {
	ad, err := netlink.NewAttributeDecoder(b)
	if err != nil {
		return nil, err
	}
	ad.ByteOrder = binary.BigEndian
	s := &SetSpec{Family: fam, Table: table}
	var keyMagic, dataMagic uint32
	isMap := false
	for ad.Next() {
		switch ad.Type() {
		case unix.NFTA_SET_NAME:
			s.Name = ad.String()
		case unix.NFTA_SET_FLAGS:
			f := ad.Uint32()
			s.Anonymous = f&unix.NFT_SET_ANONYMOUS != 0
			s.Constant = f&unix.NFT_SET_CONSTANT != 0
			s.Interval = f&unix.NFT_SET_INTERVAL != 0
			s.HasTimeout = f&unix.NFT_SET_TIMEOUT != 0
			s.Dynamic = f&unix.NFT_SET_EVAL != 0
			isMap = f&unix.NFT_SET_MAP != 0
		case unix.NFTA_SET_KEY_TYPE:
			keyMagic = ad.Uint32()
		case unix.NFTA_SET_DATA_TYPE:
			dataMagic = ad.Uint32()
		case unix.NFTA_SET_TIMEOUT:
			s.Timeout = time.Duration(ad.Uint64()) * time.Millisecond
		case unix.NFTA_SET_USERDATA:
			s.Comment, _ = userdata.GetString(ad.Bytes(), userdata.NFTNL_UDATA_SET_COMMENT)
		case 16: // NFTA_SET_HANDLE
			s.Handle = ad.Uint64()
		}
	}
	if err := ad.Err(); err != nil {
		return nil, err
	}
	var err2 error
	if s.Keys, err2 = typesFromMagic(keyMagic); err2 != nil {
		return nil, fmt.Errorf("set %s: %w", s.Name, err2)
	}
	if isMap {
		if dataMagic == verdictMagic {
			s.Data = []DataType{dataTypes["verdict"]}
		} else if s.Data, err2 = typesFromMagic(dataMagic); err2 != nil {
			return nil, fmt.Errorf("map %s: %w", s.Name, err2)
		}
	}
	return s, nil
}

func typesFromMagic(m uint32) ([]DataType, error) {
	var out []DataType
	for m != 0 {
		t, ok := typeByMagic[m&0x3f]
		if !ok {
			return nil, fmt.Errorf("unsupported datatype %d", m&0x3f)
		}
		out = append([]DataType{t}, out...)
		m >>= 6
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("missing datatype")
	}
	return out, nil
}

func (b *netlinkBackend) AddTable(fam Family, name, _ string, create bool) {
	if create {
		b.conn.CreateTable(nlTable(fam, name))
		return
	}
	b.conn.AddTable(nlTable(fam, name))
}

func (b *netlinkBackend) DelTable(fam Family, name string) { b.conn.DelTable(nlTable(fam, name)) }

var hookNums = map[string]nftables.ChainHook{"prerouting": 0, "input": 1, "forward": 2, "output": 3, "postrouting": 4}

func (b *netlinkBackend) AddChain(ch *ChainSpec, _ bool) {
	c := &nftables.Chain{Name: ch.Name, Table: nlTable(ch.Family, ch.Table)}
	if ch.Type != "" {
		hook := hookNums[ch.Hook]
		prio := nftables.ChainPriority(ch.Priority)
		c.Type, c.Hooknum, c.Priority, c.Device = nftables.ChainType(ch.Type), &hook, &prio, ch.Device
		if ch.Policy != "" {
			p := nftables.ChainPolicyAccept
			if ch.Policy == "drop" {
				p = nftables.ChainPolicyDrop
			}
			c.Policy = &p
		}
	}
	b.conn.AddChain(c)
}

func (b *netlinkBackend) FlushChain(fam Family, table, name string) {
	b.conn.FlushChain(&nftables.Chain{Name: name, Table: nlTable(fam, table)})
}

func (b *netlinkBackend) DelChain(fam Family, table, name string) {
	b.conn.DelChain(&nftables.Chain{Name: name, Table: nlTable(fam, table)})
}

func (b *netlinkBackend) AddSet(s *SetSpec, _ bool) {
	if err := b.conn.AddSet(nlSet(s), nil); err != nil {
		b.fail(err)
	}
}

func (b *netlinkBackend) FlushSet(s *SetSpec) { b.conn.FlushSet(nlSet(s)) }
func (b *netlinkBackend) DelSet(s *SetSpec)   { b.conn.DelSet(nlSet(s)) }

func (b *netlinkBackend) AddElements(s *SetSpec, els []Element) {
	if err := b.conn.SetAddElements(nlSet(s), nlElements(els)); err != nil {
		b.fail(err)
	}
}

func (b *netlinkBackend) DelElements(s *SetSpec, els []Element) {
	if err := b.conn.SetDeleteElements(nlSet(s), nlElements(els)); err != nil {
		b.fail(err)
	}
}

func (b *netlinkBackend) AddRule(r *RuleSpec) { b.addRule(r, false) }

func (b *netlinkBackend) addRule(r *RuleSpec, insert bool) {
	anon := make([]*nftables.Set, len(r.AnonSets))
	for i, a := range r.AnonSets {
		anon[i] = nlSet(&a.Spec)
		if err := b.conn.AddSet(anon[i], nlElements(a.Elements)); err != nil {
			b.fail(err)
			return
		}
	}
	exprs := make([]expr.Any, 0, len(r.Exprs))
	for _, e := range r.Exprs {
		x, err := nlExpr(e, anon)
		if err != nil {
			b.fail(err)
			return
		}
		exprs = append(exprs, x)
	}
	nr := &nftables.Rule{Table: nlTable(r.Family, r.Table), Chain: &nftables.Chain{Name: r.Chain}, Exprs: exprs}
	if r.Comment != "" {
		if len(r.Comment) > 254 {
			b.fail(fmt.Errorf("nft rule comment exceeds 254-byte userdata item limit"))
			return
		}
		nr.UserData = userdata.AppendString(nr.UserData, userdata.TypeComment, r.Comment)
	}
	if len(r.JSONExpr) > 0 {
		var err error
		nr.UserData, err = appendRuleJSON(nr.UserData, r.JSONExpr)
		if err != nil {
			b.fail(err)
			return
		}
	}
	if len(nr.UserData) > unix.NFT_USERDATA_MAXLEN {
		b.fail(fmt.Errorf("nft rule userdata exceeds %d-byte kernel limit", unix.NFT_USERDATA_MAXLEN))
		return
	}
	if insert {
		b.conn.InsertRule(nr)
	} else {
		b.conn.AddRule(nr)
	}
}

func (b *netlinkBackend) InsertRule(r *RuleSpec) {
	// Use the same expression and userdata encoder as AddRule, then change
	// the queued netlink operation to INSERT before flushing.
	b.addRule(r, true)
}

// Rule JSON is kept in rule userdata so netavark can find its rules on the
// next invocation. Linux caps the entire NFTA_RULE_USERDATA at 256 bytes;
// chunking TLVs does not bypass that cap. Compress longer expressions and
// reject anything that still cannot fit before sending a netlink batch.
func appendRuleJSON(dst, src []byte) ([]byte, error) {
	if len(src) > 1<<16 {
		return nil, fmt.Errorf("netavark rule JSON exceeds 64 KiB limit")
	}
	if len(dst)+2+len(src) <= unix.NFT_USERDATA_MAXLEN && len(src) <= 255 {
		return userdata.Append(dst, 128, src), nil
	}
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if len(dst)+2+compressed.Len() > unix.NFT_USERDATA_MAXLEN || compressed.Len() > 255 {
		return nil, fmt.Errorf("netavark rule JSON exceeds %d-byte kernel userdata limit after compression", unix.NFT_USERDATA_MAXLEN)
	}
	return userdata.Append(dst, 127, compressed.Bytes()), nil
}

func readRuleJSON(src []byte) []byte {
	if compressed := userdata.Get(src, 127); len(compressed) > 0 {
		r, err := zlib.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil
		}
		defer r.Close()
		out, err := io.ReadAll(io.LimitReader(r, 1<<16+1))
		if err != nil || len(out) > 1<<16 {
			return nil
		}
		return out
	}
	// Read rules written by earlier builds, which stored multiple TLVs.
	var out []byte
	for i := 0; i < 128; i++ {
		chunk := userdata.Get(src, userdata.Type(128+i))
		if len(chunk) == 0 {
			break
		}
		out = append(out, chunk...)
	}
	return out
}

func (b *netlinkBackend) DelRule(fam Family, table, chain string, handle uint64) {
	if err := b.conn.DelRule(&nftables.Rule{Table: nlTable(fam, table), Chain: &nftables.Chain{Name: chain}, Handle: handle}); err != nil {
		b.fail(err)
	}
}

func nlExpr(e Expr, anon []*nftables.Set) (expr.Any, error) {
	switch x := e.(type) {
	case Payload:
		base := expr.PayloadBaseNetworkHeader
		if x.Base == BaseTransport {
			base = expr.PayloadBaseTransportHeader
		}
		return &expr.Payload{DestRegister: x.DReg, Base: base, Offset: x.Offset, Len: x.Len}, nil
	case Meta:
		keys := map[string]expr.MetaKey{"l4proto": expr.MetaKeyL4PROTO, "mark": expr.MetaKeyMARK, "iifname": expr.MetaKeyIIFNAME, "oifname": expr.MetaKeyOIFNAME}
		if x.Set {
			return &expr.Meta{Key: keys[x.Key], SourceRegister: true, Register: x.SReg}, nil
		}
		return &expr.Meta{Key: keys[x.Key], Register: x.DReg}, nil
	case Ct:
		key := expr.CtKeySTATE
		if x.Key == "status" {
			key = expr.CtKeySTATUS
		} else if x.Key == "mark" {
			key = expr.CtKeyMARK
		}
		if x.Set {
			return &expr.Ct{Register: x.SReg, SourceRegister: true, Key: key}, nil
		}
		return &expr.Ct{Register: x.DReg, Key: key}, nil
	case Fib:
		return &expr.Fib{Register: x.DReg, ResultADDRTYPE: true, FlagSADDR: x.SAddr, FlagDADDR: x.DAddr}, nil
	case Numgen:
		return &expr.Numgen{Register: x.DReg, Modulus: x.Mod, Type: unix.NFT_NG_RANDOM}, nil
	case Cmp:
		op := expr.CmpOpEq
		if x.Op == "neq" {
			op = expr.CmpOpNeq
		}
		return &expr.Cmp{Op: op, Register: x.Reg, Data: x.Data}, nil
	case Bitwise:
		return &expr.Bitwise{SourceRegister: x.SReg, DestRegister: x.DReg, Len: x.Len, Mask: x.Mask, Xor: x.Xor}, nil
	case Lookup:
		l := &expr.Lookup{SourceRegister: x.SReg, DestRegister: x.DReg, IsDestRegSet: x.HasDReg, SetName: x.Set, Invert: x.Invert}
		if x.Set == "" {
			l.SetName, l.SetID = anon[x.Anon].Name, anon[x.Anon].ID
		}
		return l, nil
	case Dynset:
		op := uint32(unix.NFT_DYNSET_OP_UPDATE)
		if x.Op == "add" {
			op = unix.NFT_DYNSET_OP_ADD
		}
		return &expr.Dynset{SrcRegKey: x.SReg, SetName: x.Set, Operation: op}, nil
	case Immediate:
		return &expr.Immediate{Register: x.DReg, Data: x.Data}, nil
	case VerdictExpr:
		return nlVerdict(&x.Verdict), nil
	case Nat:
		n := &expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: x.RegAddr, RegProtoMin: x.RegProto, Specified: x.RegProto != 0}
		if x.Type == "snat" {
			n.Type = expr.NATTypeSourceNAT
		}
		if x.Family == FamilyIP6 {
			n.Family = unix.NFPROTO_IPV6
		}
		return n, nil
	case Masq:
		return &expr.Masq{FullyRandom: x.FullyRandom, Random: x.Random, Persistent: x.Persistent}, nil
	case Reject:
		return &expr.Reject{Type: x.Type, Code: x.Code}, nil
	case Counter:
		return &expr.Counter{}, nil
	case ObjRef:
		return &expr.Objref{Type: 1, Name: x.Name}, nil // NFT_OBJECT_COUNTER
	}
	return nil, fmt.Errorf("internal: unhandled expression %T", e)
}

func (b *netlinkBackend) AddCounter(c *CounterSpec, _ bool) {
	b.conn.AddObj(&nftables.CounterObj{Table: nlTable(c.Family, c.Table), Name: c.Name, Packets: c.Packets, Bytes: c.Bytes})
}

func (b *netlinkBackend) DelCounter(fam Family, table, name string) {
	b.conn.DeleteObject(&nftables.CounterObj{Table: nlTable(fam, table), Name: name})
}

// fail records a serialization error; Commit reports it before sending.
func (b *netlinkBackend) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *netlinkBackend) Commit() error {
	if b.err != nil {
		return b.err
	}
	return b.conn.Flush()
}
