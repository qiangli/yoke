// Package nft is a pure-Go implementation of the nft(8) subsets used by
// knftables and netavark: text transactions for kube-proxy/flannel, JSON
// transactions for Podman bridge networking, and JSON listing/readback.
//
// The package is split so the hard part is testable on any host:
//
//   - lex.go / parse.go turn a script into commands;
//   - compile.go turns a rule's text into a register-level IR (the same
//     expression shapes `nft --debug=netlink` prints) and encodes set
//     elements by their declared datatypes;
//   - run.go applies commands to a Backend as ONE atomic batch;
//   - list.go renders backend state as nft's JSON schema v1.
//
// Only netlink_linux.go touches the kernel (github.com/google/nftables,
// Apache-2.0). Anything outside the subset fails loudly with a parse error
// rather than being silently approximated. No nftables (GPL) source was
// consulted or ported; behavior follows the nft(8) manual and the knftables
// sources that define the contract.
package nft

import (
	"encoding/json"
	"time"
)

// Family is an nftables address family as spelled on the command line.
type Family string

const (
	FamilyIP   Family = "ip"
	FamilyIP6  Family = "ip6"
	FamilyInet Family = "inet"
)

func validFamily(f string) bool {
	switch Family(f) {
	case FamilyIP, FamilyIP6, FamilyInet:
		return true
	}
	return false
}

// DataType is one nft set/map datatype (or one field of a concatenation).
type DataType struct {
	Name string
	Len  int // bytes on the wire (unpadded)
}

var dataTypes = map[string]DataType{
	"ipv4_addr":    {"ipv4_addr", 4},
	"ipv6_addr":    {"ipv6_addr", 16},
	"inet_proto":   {"inet_proto", 1},
	"inet_service": {"inet_service", 2},
	"mark":         {"mark", 4},
	"integer":      {"integer", 4},
	"ifname":       {"ifname", 16},
	"verdict":      {"verdict", 0},
}

// padLen is a concatenation field's register footprint: nft aligns every
// field of a concatenation to 32 bits.
func padLen(n int) int { return (n + 3) &^ 3 }

// keyLen is the encoded length of a (possibly concatenated) key.
func keyLen(types []DataType) int {
	if len(types) == 1 {
		return types[0].Len
	}
	n := 0
	for _, t := range types {
		n += padLen(t.Len)
	}
	return n
}

// SetSpec describes a named or anonymous set/map.
type SetSpec struct {
	Family     Family
	Table      string
	Name       string // empty for an anonymous set
	Keys       []DataType
	Data       []DataType // nil for a set; [verdict] for a vmap
	Anonymous  bool
	Constant   bool
	Interval   bool
	Dynamic    bool
	Timeout    time.Duration
	HasTimeout bool
	Comment    string
	Handle     uint64
}

// IsMap reports whether the set carries data.
func (s *SetSpec) IsMap() bool { return len(s.Data) > 0 }

// IsVerdictMap reports whether the data is a verdict.
func (s *SetSpec) IsVerdictMap() bool { return len(s.Data) == 1 && s.Data[0].Name == "verdict" }

// Verdict is an nftables verdict: accept, drop, return, continue, jump X, goto X.
type Verdict struct {
	Kind  string
	Chain string
}

// Element is one encoded set/map element.
type Element struct {
	Key         []byte
	IntervalEnd bool
	Data        []byte   // map data (non-verdict maps)
	Verdict     *Verdict // vmap data
	Comment     string
}

// Payload bases.
const (
	BaseNetwork   = "network"
	BaseTransport = "transport"
)

// Expr is one register-level expression of a compiled rule. Exactly the
// shapes nft itself generates for the supported grammar.
type Expr interface{ isExpr() }

type (
	// Payload loads Len bytes at Offset of Base into DReg.
	Payload struct {
		Base   string
		Offset uint32
		Len    uint32
		DReg   uint32
	}
	// Meta loads (or, with Set, stores from SReg) a meta key.
	Meta struct {
		Key  string // l4proto, mark, iifname, oifname
		DReg uint32
		Set  bool
		SReg uint32
	}
	// Ct loads a conntrack key.
	Ct struct {
		Key  string // state, status
		DReg uint32
		Set  bool
		SReg uint32
	}
	// Fib looks up the route type of saddr/daddr.
	Fib struct {
		SAddr bool
		DAddr bool
		DReg  uint32
	}
	// Numgen generates a number in [0, Mod).
	Numgen struct {
		Mod  uint32
		DReg uint32
	}
	// Cmp compares Reg with Data.
	Cmp struct {
		Op   string // eq, neq
		Reg  uint32
		Data []byte
	}
	// Bitwise computes DReg = (SReg & Mask) ^ Xor.
	Bitwise struct {
		SReg, DReg uint32
		Len        uint32
		Mask, Xor  []byte
	}
	// Lookup tests SReg against a set (Anon indexes Rule.AnonSets when
	// Set is empty); a map lookup writes DReg (0 = verdict).
	Lookup struct {
		SReg    uint32
		DReg    uint32
		HasDReg bool
		Set     string
		Anon    int
		Invert  bool
	}
	// Dynset updates a dynamic set from the key in SReg.
	Dynset struct {
		Op   string // update
		SReg uint32
		Set  string
	}
	// Immediate loads constant Data into DReg.
	Immediate struct {
		DReg uint32
		Data []byte
	}
	// VerdictExpr terminates (or continues) evaluation.
	VerdictExpr struct{ Verdict }
	// Nat is dnat/snat with address/port registers.
	Nat struct {
		Type     string // dnat, snat
		Family   Family
		RegAddr  uint32
		RegProto uint32
	}
	// Masq is masquerade.
	Masq struct {
		FullyRandom bool
		Random      bool
		Persistent  bool
	}
	// Reject is the default reject (ICMP port unreachable).
	Reject struct {
		Type uint32
		Code uint8
	}
	// Counter is an anonymous counter.
	Counter struct{}
	// ObjRef references a named counter.
	ObjRef struct{ Name string }
)

func (Payload) isExpr()     {}
func (Meta) isExpr()        {}
func (Ct) isExpr()          {}
func (Fib) isExpr()         {}
func (Numgen) isExpr()      {}
func (Cmp) isExpr()         {}
func (Bitwise) isExpr()     {}
func (Lookup) isExpr()      {}
func (Dynset) isExpr()      {}
func (Immediate) isExpr()   {}
func (VerdictExpr) isExpr() {}
func (Nat) isExpr()         {}
func (Masq) isExpr()        {}
func (Reject) isExpr()      {}
func (Counter) isExpr()     {}
func (ObjRef) isExpr()      {}

// RuleSpec is a compiled rule.
type RuleSpec struct {
	Family   Family
	Table    string
	Chain    string
	Exprs    []Expr
	AnonSets []*AnonSet
	Comment  string
	Handle   uint64
	JSONExpr json.RawMessage // original JSON statements for netavark readback
}

// AnonSet is an anonymous set built for one rule.
type AnonSet struct {
	Spec     SetSpec
	Elements []Element
}

// ChainSpec describes a chain; Type is empty for a regular chain.
type ChainSpec struct {
	Family   Family
	Table    string
	Name     string
	Type     string // filter, nat, route
	Hook     string
	Priority int32
	Policy   string // accept, drop, or empty
	Device   string
	Comment  string
	Handle   uint64
}

// CounterSpec is a named counter object.
type CounterSpec struct {
	Family  Family
	Table   string
	Name    string
	Packets uint64
	Bytes   uint64
	Comment string
	Handle  uint64
}

// TableState is what the backend reports for `list table`.
type TableState struct {
	Family   Family
	Name     string
	Handle   uint64
	Comment  string
	Chains   []ChainSpec
	Sets     []SetSpec
	Counters []CounterSpec
	Rules    []RuleSpec // Chain, Handle and Comment are populated
}
