package nft

import (
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// Version is what `nft --version` reports. knftables requires >= v1.0.1.
const Version = "1.1.1"

// ListArgs selects what `nft --json list …` prints.
type ListArgs struct {
	What   string // table, chain, set, map, counters
	Family Family
	Table  string
	Name   string // chain / set / map name
	Terse  bool   // omit set elements
}

type obj = map[string]any

// ListJSON renders the selected objects in nft's JSON schema v1:
// {"nftables":[{"metainfo":{…}}, {"table":{…}}, {"chain":{…}}, …]}.
// Rules carry family/table/chain/handle/comment but no "expr" decompilation.
func ListJSON(b Backend, a ListArgs) ([]byte, error) {
	out := []obj{{"metainfo": obj{"version": Version, "release_name": "bashy", "json_schema_version": 1}}}
	switch a.What {
	case "set", "map":
		// Fetch just the one set: kube-proxy lists its maps on every full sync.
		s, err := b.GetSet(a.Family, a.Table, a.Name)
		if err != nil {
			return nil, err
		}
		if s.IsMap() != (a.What == "map") {
			return nil, notFound("%s %s", a.What, a.Name)
		}
		o, err := setObj(b, s, true)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
		return marshalList(out)
	case "counters":
		cs, err := b.GetCounters(a.Family, a.Table)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			out = append(out, obj{"counter": counterObj(c)})
		}
		return marshalList(out)
	}
	st, err := b.GetTable(a.Family, a.Table)
	if err != nil {
		return nil, err
	}
	switch a.What {
	case "table":
		out = append(out, obj{"table": tableObj(st)})
		for _, c := range st.Chains {
			out = append(out, obj{"chain": chainObj(c)})
		}
		for i := range st.Sets {
			o, err := setObj(b, &st.Sets[i], !a.Terse)
			if err != nil {
				return nil, err
			}
			out = append(out, o)
		}
		for _, c := range st.Counters {
			out = append(out, obj{"counter": counterObj(c)})
		}
		for _, r := range st.Rules {
			out = append(out, obj{"rule": ruleObj(r)})
		}
	case "chain":
		found := false
		for _, c := range st.Chains {
			if c.Name == a.Name {
				out = append(out, obj{"chain": chainObj(c)})
				found = true
			}
		}
		if !found {
			return nil, notFound("chain %s", a.Name)
		}
		for _, r := range st.Rules {
			if r.Chain == a.Name {
				out = append(out, obj{"rule": ruleObj(r)})
			}
		}
	default:
		return nil, fmt.Errorf("unsupported: list %s", a.What)
	}
	return marshalList(out)
}

func marshalList(out []obj) ([]byte, error) {
	j, err := json.Marshal(obj{"nftables": out})
	if err != nil {
		return nil, err
	}
	return append(j, '\n'), nil
}

func tableObj(t *TableState) obj {
	o := obj{"family": string(t.Family), "name": t.Name, "handle": t.Handle}
	if t.Comment != "" {
		o["comment"] = t.Comment
	}
	return o
}

func chainObj(c ChainSpec) obj {
	o := obj{"family": string(c.Family), "table": c.Table, "name": c.Name, "handle": c.Handle}
	if c.Type != "" {
		o["type"], o["hook"], o["prio"] = c.Type, c.Hook, c.Priority
		policy := c.Policy
		if policy == "" {
			policy = "accept"
		}
		o["policy"] = policy
		if c.Device != "" {
			o["dev"] = c.Device
		}
	}
	if c.Comment != "" {
		o["comment"] = c.Comment
	}
	return o
}

func counterObj(c CounterSpec) obj {
	o := obj{"family": string(c.Family), "name": c.Name, "table": c.Table, "handle": c.Handle, "packets": c.Packets, "bytes": c.Bytes}
	if c.Comment != "" {
		o["comment"] = c.Comment
	}
	return o
}

func ruleObj(r RuleSpec) obj {
	o := obj{"family": string(r.Family), "table": r.Table, "chain": r.Chain, "handle": r.Handle}
	if len(r.JSONExpr) > 0 {
		o["expr"] = json.RawMessage(r.JSONExpr)
	} else {
		o["expr"] = []any{}
	}
	if r.Comment != "" {
		o["comment"] = r.Comment
	}
	return o
}

func typeJSON(ts []DataType) any {
	if len(ts) == 1 {
		return ts[0].Name
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return names
}

func setObj(b Backend, s *SetSpec, withElems bool) (obj, error) {
	o := obj{"family": string(s.Family), "name": s.Name, "table": s.Table, "type": typeJSON(s.Keys), "handle": s.Handle}
	kind := "set"
	if s.IsMap() {
		kind = "map"
		o["map"] = typeJSON(s.Data)
	}
	var flags []string
	if s.Constant {
		flags = append(flags, "constant")
	}
	if s.Interval {
		flags = append(flags, "interval")
	}
	if s.HasTimeout {
		flags = append(flags, "timeout")
	}
	if s.Dynamic {
		flags = append(flags, "dynamic")
	}
	if flags != nil {
		o["flags"] = flags
	}
	if s.Timeout > 0 {
		o["timeout"] = int64(s.Timeout / time.Second)
	}
	if s.Comment != "" {
		o["comment"] = s.Comment
	}
	if withElems {
		els, err := b.GetElements(s)
		if err != nil {
			return nil, err
		}
		if js := elemsJSON(s, els); len(js) > 0 {
			o["elem"] = js
		}
	}
	return obj{kind: o}, nil
}

func verdictJSON(v *Verdict) any {
	if v.Chain != "" {
		return obj{v.Kind: obj{"target": v.Chain}}
	}
	return obj{v.Kind: nil}
}

func elemsJSON(s *SetSpec, els []Element) []any {
	var out []any
	if s.Interval {
		for i := 0; i < len(els); i++ {
			if els[i].IntervalEnd {
				continue
			}
			lo := new(big.Int).SetBytes(els[i].Key)
			hi := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(len(els[i].Key)*8)), big.NewInt(1))
			if i+1 < len(els) && els[i+1].IntervalEnd {
				hi = new(big.Int).Sub(new(big.Int).SetBytes(els[i+1].Key), big.NewInt(1))
			}
			out = append(out, rangeJSON(s.Keys[0], els[i].Key, lo, hi))
		}
		return out
	}
	for _, e := range els {
		var key any = decodeConcat(s.Keys, e.Key)
		if e.Comment != "" {
			key = obj{"elem": obj{"val": key, "comment": e.Comment}}
		}
		switch {
		case s.IsVerdictMap() && e.Verdict != nil:
			out = append(out, []any{key, verdictJSON(e.Verdict)})
		case s.IsMap():
			out = append(out, []any{key, decodeConcat(s.Data, e.Data)})
		default:
			out = append(out, key)
		}
	}
	return out
}

// rangeJSON renders [lo, hi] as a single value, a prefix, or a range.
func rangeJSON(t DataType, raw []byte, lo, hi *big.Int) any {
	n := len(raw)
	enc := func(x *big.Int) any { return decodeValue(t, x.FillBytes(make([]byte, n))) }
	if lo.Cmp(hi) == 0 {
		return enc(lo)
	}
	size := new(big.Int).Add(new(big.Int).Sub(hi, lo), big.NewInt(1))
	if size.BitLen() > 0 && new(big.Int).And(size, new(big.Int).Sub(size, big.NewInt(1))).Sign() == 0 {
		host := size.BitLen() - 1
		if new(big.Int).Rsh(lo, uint(host)).Lsh(new(big.Int).Rsh(lo, uint(host)), uint(host)).Cmp(lo) == 0 {
			return obj{"prefix": obj{"addr": enc(lo), "len": n*8 - host}}
		}
	}
	return obj{"range": []any{enc(lo), enc(hi)}}
}
