package nft

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
)

var protoNumbers = map[string]byte{
	"icmp": 1, "igmp": 2, "tcp": 6, "udp": 17, "ipv6-icmp": 58, "icmpv6": 58, "sctp": 132, "udplite": 136,
}

var protoNames = map[byte]string{1: "icmp", 2: "igmp", 6: "tcp", 17: "udp", 58: "ipv6-icmp", 132: "sctp", 136: "udplite"}

// encodeValue encodes one literal of datatype dt. A prefix ("10.0.0.0/8")
// is returned as the network address plus its length in bits; plen is -1
// for a non-prefix value.
func encodeValue(dt DataType, s string) (b []byte, plen int, err error) {
	plen = -1
	switch dt.Name {
	case "ipv4_addr", "ipv6_addr":
		bits := 32
		if dt.Name == "ipv6_addr" {
			bits = 128
		}
		addr := s
		if i := strings.IndexByte(s, '/'); i >= 0 {
			p, perr := netip.ParsePrefix(s)
			if perr != nil {
				return nil, 0, fmt.Errorf("bad prefix %q", s)
			}
			if p.Addr().BitLen() != bits {
				return nil, 0, fmt.Errorf("%q is not a %s prefix", s, dt.Name)
			}
			p = p.Masked()
			addr = p.Addr().String()
			plen = p.Bits()
			if plen == bits {
				plen = -1
			}
		}
		a, perr := netip.ParseAddr(addr)
		if perr != nil || a.BitLen() != bits || a.Zone() != "" {
			return nil, 0, fmt.Errorf("bad %s %q", dt.Name, s)
		}
		return a.AsSlice(), plen, nil
	case "inet_proto":
		if n, ok := protoNumbers[s]; ok {
			return []byte{n}, -1, nil
		}
		n, perr := strconv.ParseUint(s, 0, 8)
		if perr != nil {
			return nil, 0, fmt.Errorf("bad protocol %q", s)
		}
		return []byte{byte(n)}, -1, nil
	case "inet_service":
		n, perr := strconv.ParseUint(s, 0, 16)
		if perr != nil {
			return nil, 0, fmt.Errorf("bad port %q", s)
		}
		return binary.BigEndian.AppendUint16(nil, uint16(n)), -1, nil
	case "mark", "integer":
		n, perr := strconv.ParseUint(s, 0, 32)
		if perr != nil {
			return nil, 0, fmt.Errorf("bad %s %q", dt.Name, s)
		}
		return binary.NativeEndian.AppendUint32(nil, uint32(n)), -1, nil
	case "ifname":
		if len(s) >= 16 {
			return nil, 0, fmt.Errorf("interface name %q too long", s)
		}
		b := make([]byte, 16)
		copy(b, s)
		return b, -1, nil
	}
	return nil, 0, fmt.Errorf("cannot encode %s value %q", dt.Name, s)
}

// decodeValue renders an encoded value as nft's JSON does.
func decodeValue(dt DataType, b []byte) any {
	switch dt.Name {
	case "ipv4_addr", "ipv6_addr":
		if a, ok := netip.AddrFromSlice(b); ok {
			return a.String()
		}
	case "inet_proto":
		if len(b) >= 1 {
			if n, ok := protoNames[b[0]]; ok {
				return n
			}
			return int(b[0])
		}
	case "inet_service":
		if len(b) >= 2 {
			return int(binary.BigEndian.Uint16(b))
		}
	case "mark", "integer":
		if len(b) >= 4 {
			return int64(binary.NativeEndian.Uint32(b))
		}
	case "ifname":
		return strings.TrimRight(string(b), "\x00")
	}
	return fmt.Sprintf("0x%x", b)
}

// prefixMask returns a len-byte mask with the leading bits set.
func prefixMask(n, bits int) []byte {
	m := make([]byte, n)
	for i := 0; i < bits; i++ {
		m[i/8] |= 0x80 >> (i % 8)
	}
	return m
}

// prefixRange returns the first and last address of addr/plen.
func prefixRange(addr []byte, plen int) (first, last []byte) {
	first = append([]byte{}, addr...)
	last = append([]byte{}, addr...)
	for i := plen; i < len(addr)*8; i++ {
		last[i/8] |= 0x80 >> (i % 8)
	}
	return first, last
}

// encodeConcat encodes fields of a (possibly concatenated) key or data,
// padding every field to 32 bits when there is more than one.
func encodeConcat(types []DataType, fields [][]token) ([]byte, error) {
	if len(fields) != len(types) {
		return nil, fmt.Errorf("element %q has %d field(s), type has %d", joinFields(fields), len(fields), len(types))
	}
	var out []byte
	for i, f := range fields {
		if len(f) != 1 {
			return nil, fmt.Errorf("syntax error in element field %q", joinToks(f))
		}
		b, plen, err := encodeValue(types[i], f[0].val)
		if err != nil {
			return nil, err
		}
		if plen >= 0 {
			return nil, fmt.Errorf("prefix %q needs an interval set", f[0].val)
		}
		if len(types) > 1 {
			b = append(b, make([]byte, padLen(len(b))-len(b))...)
		}
		out = append(out, b...)
	}
	return out, nil
}

func joinFields(fields [][]token) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = joinToks(f)
	}
	return strings.Join(parts, " . ")
}

// decodeConcat splits an encoded key/data into JSON values.
func decodeConcat(types []DataType, b []byte) any {
	if len(types) == 1 {
		return decodeValue(types[0], b)
	}
	var parts []any
	off := 0
	for _, t := range types {
		if off+t.Len > len(b) {
			break
		}
		parts = append(parts, decodeValue(t, b[off:off+t.Len]))
		off += padLen(t.Len)
	}
	return map[string]any{"concat": parts}
}

// parseVerdict parses "accept", "drop", "goto X", … from tokens.
func parseVerdict(t []token) (*Verdict, error) {
	if len(t) == 1 {
		switch t[0].val {
		case "accept", "drop", "return", "continue":
			return &Verdict{Kind: t[0].val}, nil
		}
	}
	if len(t) == 2 && (t[0].is("jump") || t[0].is("goto")) && t[1].kind != tPunct {
		return &Verdict{Kind: t[0].val, Chain: t[1].val}, nil
	}
	return nil, fmt.Errorf("bad verdict %q", joinToks(t))
}

// intervalElements turns prefixes/addresses into the start / end-exclusive
// element pairs an interval set stores, merging overlaps.
func intervalElements(ranges [][2][]byte) []Element {
	type rng struct{ lo, hi *big.Int }
	n := len(ranges[0][0])
	var rs []rng
	for _, r := range ranges {
		rs = append(rs, rng{new(big.Int).SetBytes(r[0]), new(big.Int).SetBytes(r[1])})
	}
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].lo.Cmp(rs[j-1].lo) < 0; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
	var merged []rng
	one := big.NewInt(1)
	for _, r := range rs {
		if k := len(merged) - 1; k >= 0 && new(big.Int).Add(merged[k].hi, one).Cmp(r.lo) >= 0 {
			if r.hi.Cmp(merged[k].hi) > 0 {
				merged[k].hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(one, uint(n*8)), one)
	var out []Element
	for _, r := range merged {
		out = append(out, Element{Key: r.lo.FillBytes(make([]byte, n))})
		if r.hi.Cmp(max) < 0 {
			end := new(big.Int).Add(r.hi, one)
			out = append(out, Element{Key: end.FillBytes(make([]byte, n)), IntervalEnd: true})
		}
	}
	return out
}
