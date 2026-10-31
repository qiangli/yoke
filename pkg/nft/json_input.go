package nft

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JSONToText translates the JSON transaction form emitted by netavark's
// nftables Batch into the validated nft command subset. Unknown objects and
// expressions are errors, never silently omitted.
func JSONToText(input []byte) (string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(input, &root); err != nil {
		return "", err
	}
	if len(root) != 1 || len(root["nftables"]) == 0 {
		return "", fmt.Errorf("JSON transaction requires only nftables array")
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(input, &doc); err != nil {
		return "", err
	}
	if len(doc.Nftables) == 0 {
		return "", fmt.Errorf("empty nftables transaction")
	}
	var lines []string
	for i, wrapper := range doc.Nftables {
		if len(wrapper) != 1 {
			return "", fmt.Errorf("nftables[%d]: expected one operation", i)
		}
		for verb, raw := range wrapper {
			if verb != "add" && verb != "create" && verb != "delete" && verb != "insert" {
				return "", fmt.Errorf("unsupported JSON operation %q", verb)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil || len(object) != 1 {
				return "", fmt.Errorf("nftables[%d]: invalid object", i)
			}
			for kind, body := range object {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					return "", err
				}
				get := func(k string) string { var s string; _ = json.Unmarshal(fields[k], &s); return s }
				family, table := get("family"), get("table")
				if kind == "table" {
					table = get("name")
				}
				if !validFamily(family) || table == "" {
					return "", fmt.Errorf("%s: missing family or table", kind)
				}
				prefix := verb + " " + kind + " " + family + " " + table
				if kind != "table" {
					prefix += " " + get("name")
				}
				var line string
				switch kind {
				case "table":
					if err := allowJSONFields(fields, "family", "name", "handle"); err != nil {
						return "", err
					}
					line = prefix
				case "chain":
					if err := allowJSONFields(fields, "family", "table", "name", "type", "hook", "prio", "policy", "handle"); err != nil {
						return "", err
					}
					if get("name") == "" {
						return "", fmt.Errorf("chain name missing")
					}
					line = prefix
					if typ := get("type"); typ != "" {
						var prio int32
						if err := json.Unmarshal(fields["prio"], &prio); err != nil {
							return "", fmt.Errorf("chain priority: %w", err)
						}
						line += fmt.Sprintf(" { type %s hook %s priority %d ; policy %s ; }", typ, get("hook"), prio, get("policy"))
					}
				case "rule":
					if verb == "delete" {
						if err := allowJSONFields(fields, "family", "table", "chain", "handle", "expr", "comment"); err != nil {
							return "", err
						}
						var handle uint64
						if err := json.Unmarshal(fields["handle"], &handle); err != nil || handle == 0 {
							return "", fmt.Errorf("delete rule needs handle")
						}
						line = fmt.Sprintf("delete rule %s %s %s handle %d", family, table, get("chain"), handle)
						break
					}
					if verb != "add" && verb != "insert" {
						return "", fmt.Errorf("unsupported JSON %s rule", verb)
					}
					if err := allowJSONFields(fields, "family", "table", "chain", "expr", "comment"); err != nil {
						return "", err
					}
					var exprs []map[string]json.RawMessage
					if err := json.Unmarshal(fields["expr"], &exprs); err != nil || len(exprs) == 0 {
						return "", fmt.Errorf("rule needs expressions")
					}
					var parts []string
					for _, e := range exprs {
						s, err := jsonStatement(e)
						if err != nil {
							return "", err
						}
						parts = append(parts, s)
					}
					line = "add rule " + family + " " + table + " " + get("chain") + " " + strings.Join(parts, " ")
					if comment := get("comment"); comment != "" {
						line += " comment " + strconv.Quote(comment)
					}
				case "set", "map":
					if err := allowJSONFields(fields, "family", "table", "name", "type", "map", "flags", "handle"); err != nil {
						return "", err
					}
					if get("name") == "" {
						return "", fmt.Errorf("%s name missing", kind)
					}
					if verb == "delete" {
						line = prefix
						break
					}
					if kind == "set" && len(fields["map"]) > 0 {
						return "", fmt.Errorf("set cannot carry map datatype")
					}
					keys, err := jsonDataType(fields["type"])
					if err != nil {
						return "", err
					}
					if kind == "map" {
						data, err := jsonDataType(fields["map"])
						if err != nil {
							return "", err
						}
						keys += " : " + data
					}
					line = prefix + " { type " + keys + " ;"
					if len(fields["flags"]) > 0 {
						var flags []string
						if err := json.Unmarshal(fields["flags"], &flags); err != nil {
							return "", err
						}
						line += " flags " + strings.Join(flags, ",") + " ;"
					}
					line += " }"
				case "element":
					if err := allowJSONFields(fields, "family", "table", "name", "elem"); err != nil {
						return "", err
					}
					var elems []json.RawMessage
					if err := json.Unmarshal(fields["elem"], &elems); err != nil || len(elems) == 0 {
						return "", fmt.Errorf("element needs elem array")
					}
					var parts []string
					for _, e := range elems {
						s, err := jsonElement(e)
						if err != nil {
							return "", err
						}
						parts = append(parts, s)
					}
					line = prefix + " { " + strings.Join(parts, ", ") + " }"
				default:
					return "", fmt.Errorf("unsupported JSON object %q", kind)
				}
				lines = append(lines, line)
			}
		}
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func jsonDataType(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s, nil
	}
	var a []string
	if json.Unmarshal(raw, &a) == nil && len(a) > 0 {
		return strings.Join(a, " . "), nil
	}
	return "", fmt.Errorf("unsupported datatype %s", raw)
}

func jsonElement(raw json.RawMessage) (string, error) {
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) == nil {
		if len(pair) != 2 {
			return "", fmt.Errorf("unsupported element pair %s", raw)
		}
		key, err := jsonExpression(pair[0])
		if err != nil {
			return "", err
		}
		value, err := jsonExpression(pair[1])
		if err != nil {
			return "", err
		}
		return key + " : " + value, nil
	}
	return jsonExpression(raw)
}

// RunJSON validates and applies a JSON nftables batch atomically.
func RunJSON(b Backend, input []byte, check bool) error {
	script, err := JSONToText(input)
	if err != nil {
		return err
	}
	cmds, err := Parse(script)
	if err != nil {
		return err
	}
	var doc struct {
		Nftables []map[string]map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(input, &doc); err != nil {
		return err
	}
	for i, wrapper := range doc.Nftables {
		for verb, object := range wrapper {
			if verb != "add" && verb != "insert" {
				continue
			}
			if raw, ok := object["rule"]; ok {
				var rule struct {
					Expr json.RawMessage `json:"expr"`
				}
				if err := json.Unmarshal(raw, &rule); err != nil {
					return err
				}
				cmds[i].JSONExpr = rule.Expr
				cmds[i].Insert = verb == "insert"
			}
		}
	}
	return runCommands(b, cmds, check)
}

func allowJSONFields(m map[string]json.RawMessage, names ...string) error {
	allowed := map[string]bool{}
	for _, n := range names {
		allowed[n] = true
	}
	for n := range m {
		if !allowed[n] {
			return fmt.Errorf("unsupported JSON field %q", n)
		}
	}
	return nil
}

func checkedJSONFields(raw json.RawMessage, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	return allowJSONFields(fields, names...)
}

func jsonStatement(m map[string]json.RawMessage) (string, error) {
	if len(m) != 1 {
		return "", fmt.Errorf("invalid JSON statement")
	}
	for kind, raw := range m {
		switch kind {
		case "match":
			if err := checkedJSONFields(raw, "left", "right", "op"); err != nil {
				return "", err
			}
			var v struct {
				Left, Right json.RawMessage
				Op          string
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", err
			}
			left, err := jsonExpression(v.Left)
			if err != nil {
				return "", err
			}
			right, err := jsonExpression(v.Right)
			if err != nil {
				return "", err
			}
			if v.Op != "==" && v.Op != "!=" && v.Op != "in" {
				return "", fmt.Errorf("unsupported match operator %q", v.Op)
			}
			if v.Op == "in" {
				return left + " " + right, nil
			}
			return left + " " + v.Op + " " + right, nil
		case "accept", "drop", "return", "continue", "masquerade":
			if string(raw) != "null" {
				return "", fmt.Errorf("unsupported %s options", kind)
			}
			return kind, nil
		case "jump", "goto":
			if err := checkedJSONFields(raw, "target"); err != nil {
				return "", err
			}
			var v struct {
				Target string `json:"target"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", err
			}
			if v.Target == "" {
				return "", fmt.Errorf("%s target missing", kind)
			}
			return kind + " " + v.Target, nil
		case "mangle":
			if err := checkedJSONFields(raw, "key", "value"); err != nil {
				return "", err
			}
			var v struct{ Key, Value json.RawMessage }
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", err
			}
			key, err := jsonExpression(v.Key)
			if err != nil {
				return "", err
			}
			value, err := jsonExpression(v.Value)
			if err != nil {
				return "", err
			}
			if key != "meta mark" && key != "ct mark" {
				return "", fmt.Errorf("unsupported mangle key %q", key)
			}
			return key + " set " + value, nil
		case "dnat", "snat":
			if err := checkedJSONFields(raw, "addr", "port", "family"); err != nil {
				return "", err
			}
			var v struct {
				Addr, Port json.RawMessage
				Family     string
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", err
			}
			if v.Family != "ip" && v.Family != "ip6" {
				return "", fmt.Errorf("unsupported %s family %q", kind, v.Family)
			}
			addr, err := jsonExpression(v.Addr)
			if err != nil {
				return "", err
			}
			if len(v.Port) > 0 {
				port, err := jsonExpression(v.Port)
				if err != nil {
					return "", err
				}
				if v.Family == "ip6" {
					addr = "[" + addr + "]"
				}
				addr += ":" + port
			}
			return kind + " " + v.Family + " to " + addr, nil
		default:
			return "", fmt.Errorf("unsupported JSON statement %q", kind)
		}
	}
	return "", fmt.Errorf("empty JSON statement")
}

func jsonExpression(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var values []string
		for _, x := range list {
			s, err := jsonExpression(x)
			if err != nil {
				return "", err
			}
			values = append(values, s)
		}
		return strings.Join(values, ","), nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) != 1 {
		return "", fmt.Errorf("unsupported JSON expression %s", raw)
	}
	for kind, body := range m {
		switch kind {
		case "payload":
			if err := checkedJSONFields(body, "protocol", "field"); err != nil {
				return "", err
			}
			var p struct{ Protocol, Field string }
			if err := json.Unmarshal(body, &p); err != nil {
				return "", err
			}
			return p.Protocol + " " + p.Field, nil
		case "meta", "ct":
			if err := checkedJSONFields(body, "key"); err != nil {
				return "", err
			}
			var p struct{ Key string }
			if err := json.Unmarshal(body, &p); err != nil {
				return "", err
			}
			return kind + " " + p.Key, nil
		case "prefix":
			if err := checkedJSONFields(body, "addr", "len"); err != nil {
				return "", err
			}
			var p struct {
				Addr json.RawMessage
				Len  int
			}
			if err := json.Unmarshal(body, &p); err != nil {
				return "", err
			}
			a, err := jsonExpression(p.Addr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s/%d", a, p.Len), nil
		case "set":
			var arr []json.RawMessage
			if err := json.Unmarshal(body, &arr); err != nil {
				return "", err
			}
			var parts []string
			for _, x := range arr {
				s, err := jsonExpression(x)
				if err != nil {
					return "", err
				}
				parts = append(parts, s)
			}
			return "{ " + strings.Join(parts, ", ") + " }", nil
		case "concat":
			var arr []json.RawMessage
			if err := json.Unmarshal(body, &arr); err != nil || len(arr) == 0 {
				return "", fmt.Errorf("invalid concatenation")
			}
			var parts []string
			for _, x := range arr {
				s, err := jsonExpression(x)
				if err != nil {
					return "", err
				}
				parts = append(parts, s)
			}
			return strings.Join(parts, " . "), nil
		case "jump", "goto":
			if err := checkedJSONFields(body, "target"); err != nil {
				return "", err
			}
			var p struct{ Target string }
			if err := json.Unmarshal(body, &p); err != nil || p.Target == "" {
				return "", fmt.Errorf("invalid verdict")
			}
			return kind + " " + p.Target, nil
		case "fib":
			if err := checkedJSONFields(body, "result", "flags"); err != nil {
				return "", err
			}
			var p struct {
				Result string
				Flags  []string
			}
			if err := json.Unmarshal(body, &p); err != nil {
				return "", err
			}
			if p.Result != "type" || len(p.Flags) != 1 || (p.Flags[0] != "daddr" && p.Flags[0] != "saddr") {
				return "", fmt.Errorf("unsupported fib expression")
			}
			return "fib " + p.Flags[0] + " type", nil
		case "&", "|":
			var p []json.RawMessage
			if err := json.Unmarshal(body, &p); err != nil || len(p) != 2 {
				return "", fmt.Errorf("unsupported binary expression %q", kind)
			}
			left, err := jsonExpression(p[0])
			if err != nil {
				return "", err
			}
			right, err := jsonExpression(p[1])
			if err != nil {
				return "", err
			}
			if kind == "&" {
				return left + " and " + right, nil
			}
			return left + " or " + right, nil
		case "range":
			var p []json.RawMessage
			if err := json.Unmarshal(body, &p); err != nil || len(p) != 2 {
				return "", fmt.Errorf("invalid range")
			}
			lo, err := jsonExpression(p[0])
			if err != nil {
				return "", err
			}
			hi, err := jsonExpression(p[1])
			if err != nil {
				return "", err
			}
			return lo + "-" + hi, nil
		default:
			return "", fmt.Errorf("unsupported JSON expression %q", kind)
		}
	}
	return "", fmt.Errorf("empty JSON expression")
}
