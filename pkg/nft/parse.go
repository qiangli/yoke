package nft

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Command is one parsed script command.
type Command struct {
	Verb   string // add, create, delete, flush
	Object string // table, chain, set, map, counter, element, rule
	Family Family
	Table  string
	Name   string // chain / set / map / counter name; a rule's chain; an element's set
	Line   int

	TableComment string
	Chain        *ChainSpec
	Set          *SetSpec
	Counter      *CounterSpec
	Elements     []rawElement // element commands
	RuleToks     []token      // rule commands
	JSONExpr     []byte
	Handle       uint64
	Insert       bool
}

// rawElement is an element before encoding (encoding needs the set's types).
type rawElement struct {
	Key     [][]token // one token run per concatenation field
	Value   [][]token
	Comment string
}

// Parse parses an nft script.
func Parse(src string) ([]*Command, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	groups, err := splitCommands(toks)
	if err != nil {
		return nil, err
	}
	var cmds []*Command
	for _, g := range groups {
		c, err := parseCommand(g)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", g[0].line, err)
		}
		cmds = append(cmds, c)
	}
	return cmds, nil
}

func parseCommand(t []token) (*Command, error) {
	if len(t) < 2 {
		return nil, fmt.Errorf("syntax error: incomplete command %q", joinToks(t))
	}
	c := &Command{Verb: t[0].val, Object: t[1].val, Line: t[0].line}
	switch c.Verb {
	case "add", "create", "delete", "flush":
	default:
		return nil, fmt.Errorf("unsupported command %q", t[0].val)
	}
	rest := t[2:]
	if len(rest) < 2 || rest[0].kind != tWord || !validFamily(rest[0].val) {
		return nil, fmt.Errorf("syntax error: %s %s needs FAMILY TABLE", c.Verb, c.Object)
	}
	c.Family = Family(rest[0].val)
	c.Table = rest[1].val
	rest = rest[2:]

	switch c.Object {
	case "table":
		if c.Verb == "flush" {
			return nil, fmt.Errorf("unsupported command: flush table")
		}
		return c, parseTableBody(c, rest)
	case "chain", "set", "map", "counter", "element", "rule":
	default:
		return nil, fmt.Errorf("unsupported object %q", c.Object)
	}
	if len(rest) == 0 || rest[0].kind != tWord {
		return nil, fmt.Errorf("syntax error: %s %s needs a name", c.Verb, c.Object)
	}
	if rest[0].val == "handle" {
		return nil, fmt.Errorf("unsupported: %s %s by handle", c.Verb, c.Object)
	}
	c.Name = rest[0].val
	rest = rest[1:]

	switch c.Object {
	case "chain":
		if c.Verb == "add" || c.Verb == "create" {
			return c, parseChainBody(c, rest)
		}
	case "set", "map":
		if c.Verb == "add" || c.Verb == "create" {
			return c, parseSetBody(c, rest)
		}
	case "counter":
		if c.Verb == "add" || c.Verb == "create" {
			c.Counter = &CounterSpec{Family: c.Family, Table: c.Table, Name: c.Name}
			return c, parseCounterBody(c, rest)
		}
		if c.Verb == "flush" {
			return nil, fmt.Errorf("unsupported command: flush counter")
		}
	case "element":
		if c.Verb == "flush" {
			return nil, fmt.Errorf("unsupported command: flush element")
		}
		els, err := parseElements(rest, c.Verb == "add" || c.Verb == "create")
		c.Elements = els
		return c, err
	case "rule":
		if c.Verb == "delete" && len(rest) == 2 && rest[0].is("handle") {
			h, err := strconv.ParseUint(rest[1].val, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid rule handle %q", rest[1].val)
			}
			c.Handle = h
			return c, nil
		}
		if c.Verb != "add" {
			return nil, fmt.Errorf("unsupported command: %s rule", c.Verb)
		}
		if len(rest) > 0 && (rest[0].is("index") || rest[0].is("handle") || rest[0].is("position")) {
			return nil, fmt.Errorf("unsupported: rule %s", rest[0].val)
		}
		c.RuleToks = rest
		return c, nil
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("syntax error: unexpected %s", rest[0])
	}
	return c, nil
}

// clauses splits "{ a b ; c d ; }" into its ";"-separated clauses.
func clauses(t []token) ([][]token, error) {
	if len(t) == 0 {
		return nil, nil
	}
	if !t[0].is("{") || !t[len(t)-1].is("}") {
		return nil, fmt.Errorf("syntax error: expected { ... }, got %q", joinToks(t))
	}
	var out [][]token
	var cur []token
	for _, x := range t[1 : len(t)-1] {
		if x.kind == tPunct && x.val == ";" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, x)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

func parseTableBody(c *Command, t []token) error {
	cls, err := clauses(t)
	if err != nil {
		return err
	}
	for _, cl := range cls {
		switch {
		case cl[0].is("comment") && len(cl) == 2 && cl[1].kind == tString:
			c.TableComment = cl[1].val
		default:
			return fmt.Errorf("unsupported table option %q", joinToks(cl))
		}
	}
	return nil
}

func parseChainBody(c *Command, t []token) error {
	ch := &ChainSpec{Family: c.Family, Table: c.Table, Name: c.Name}
	c.Chain = ch
	cls, err := clauses(t)
	if err != nil {
		return err
	}
	for _, cl := range cls {
		switch {
		case cl[0].is("type"):
			// type T hook H [device "D"] priority P
			if len(cl) < 6 || !cl[2].is("hook") {
				return fmt.Errorf("syntax error in chain spec %q", joinToks(cl))
			}
			ch.Type, ch.Hook = cl[1].val, cl[3].val
			switch ch.Type {
			case "filter", "nat", "route":
			default:
				return fmt.Errorf("unsupported chain type %q", ch.Type)
			}
			i := 4
			if cl[i].is("device") {
				if i+1 >= len(cl) {
					return fmt.Errorf("syntax error in chain spec %q", joinToks(cl))
				}
				ch.Device = cl[i+1].val
				i += 2
			}
			if i+1 >= len(cl) || !cl[i].is("priority") || i+2 != len(cl) {
				return fmt.Errorf("syntax error in chain spec %q", joinToks(cl))
			}
			p, err := ParsePriority(cl[i+1].val)
			if err != nil {
				return err
			}
			ch.Priority = p
		case cl[0].is("policy") && len(cl) == 2 && (cl[1].is("accept") || cl[1].is("drop")):
			ch.Policy = cl[1].val
		case cl[0].is("comment") && len(cl) == 2 && cl[1].kind == tString:
			ch.Comment = cl[1].val
		default:
			return fmt.Errorf("unsupported chain option %q", joinToks(cl))
		}
	}
	return nil
}

var namedPriorities = map[string]int32{
	"raw": -300, "mangle": -150, "dstnat": -100, "filter": 0, "security": 50, "srcnat": 100,
}

// ParsePriority parses a numeric or named (± offset) base-chain priority.
func ParsePriority(s string) (int32, error) {
	if n, err := strconv.ParseInt(s, 10, 32); err == nil {
		return int32(n), nil
	}
	name, off := s, int64(0)
	if i := strings.IndexAny(s, "+-"); i > 0 {
		var err error
		name = s[:i]
		off, err = strconv.ParseInt(s[i:], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("bad priority %q", s)
		}
	}
	base, ok := namedPriorities[name]
	if !ok {
		return 0, fmt.Errorf("bad priority %q", s)
	}
	return base + int32(off), nil
}

func parseSetBody(c *Command, t []token) error {
	s := &SetSpec{Family: c.Family, Table: c.Table, Name: c.Name}
	c.Set = s
	cls, err := clauses(t)
	if err != nil {
		return err
	}
	if len(cls) == 0 && (c.Verb == "add" || c.Verb == "create") {
		return fmt.Errorf("%s %s needs a type", c.Verb, c.Object)
	}
	for _, cl := range cls {
		switch {
		case cl[0].is("type"):
			keys, data, err := parseTypeSpec(cl[1:])
			if err != nil {
				return err
			}
			if (c.Object == "map") != (data != nil) {
				return fmt.Errorf("syntax error: %s type %q", c.Object, joinToks(cl))
			}
			s.Keys, s.Data = keys, data
		case cl[0].is("flags"):
			for _, x := range cl[1:] {
				if x.is(",") {
					continue
				}
				for _, f := range strings.Split(x.val, ",") {
					switch f {
					case "dynamic":
						s.Dynamic = true
					case "timeout":
						s.HasTimeout = true
					case "interval":
						s.Interval = true
					case "constant":
						s.Constant = true
					case "":
					default:
						return fmt.Errorf("unsupported set flag %q", f)
					}
				}
			}
		case cl[0].is("timeout") && len(cl) == 2:
			d, err := parseDuration(cl[1].val)
			if err != nil {
				return err
			}
			s.Timeout, s.HasTimeout = d, true
		case cl[0].is("comment") && len(cl) == 2 && cl[1].kind == tString:
			s.Comment = cl[1].val
		default:
			return fmt.Errorf("unsupported %s option %q", c.Object, joinToks(cl))
		}
	}
	if len(s.Keys) == 0 {
		return fmt.Errorf("%s %s needs a type", c.Verb, c.Object)
	}
	return nil
}

// parseTypeSpec parses "T [. T]... [: T [. T]...]".
func parseTypeSpec(t []token) (keys, data []DataType, err error) {
	cur := &keys
	expectType := true
	for _, x := range t {
		switch {
		case x.is(":") && !expectType && data == nil && cur == &keys:
			cur = &data
			data = []DataType{}
			expectType = true
		case x.is(".") && !expectType:
			expectType = true
		case expectType && x.kind == tWord:
			dt, ok := dataTypes[x.val]
			if !ok {
				return nil, nil, fmt.Errorf("unsupported datatype %q", x.val)
			}
			*cur = append(*cur, dt)
			expectType = false
		default:
			return nil, nil, fmt.Errorf("syntax error in type at %s", x)
		}
	}
	if expectType {
		return nil, nil, fmt.Errorf("syntax error: incomplete type")
	}
	for _, k := range keys {
		if k.Name == "verdict" {
			return nil, nil, fmt.Errorf("verdict is only valid as map data")
		}
	}
	if len(data) > 1 {
		for _, d := range data {
			if d.Name == "verdict" {
				return nil, nil, fmt.Errorf("verdict cannot be concatenated")
			}
		}
	}
	return keys, data, nil
}

func parseCounterBody(c *Command, t []token) error {
	cls, err := clauses(t)
	if err != nil {
		return err
	}
	for _, cl := range cls {
		switch {
		case cl[0].is("comment") && len(cl) == 2 && cl[1].kind == tString:
			c.Counter.Comment = cl[1].val
		case cl[0].is("packets") && len(cl) == 4 && cl[2].is("bytes"):
			p, err1 := strconv.ParseUint(cl[1].val, 10, 64)
			b, err2 := strconv.ParseUint(cl[3].val, 10, 64)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("syntax error in counter %q", joinToks(cl))
			}
			c.Counter.Packets, c.Counter.Bytes = p, b
		default:
			return fmt.Errorf("unsupported counter option %q", joinToks(cl))
		}
	}
	return nil
}

// parseElements parses "{ k [. k]... [comment "c"] [: v [. v]...] , ... }".
func parseElements(t []token, allowValue bool) ([]rawElement, error) {
	if len(t) < 2 || !t[0].is("{") || !t[len(t)-1].is("}") {
		return nil, fmt.Errorf("syntax error: expected { elements }")
	}
	var out []rawElement
	for _, part := range splitTop(t[1:len(t)-1], ",") {
		if len(part) == 0 {
			continue
		}
		e, err := parseRawElement(part)
		if err != nil {
			return nil, err
		}
		if !allowValue && (e.Value != nil || e.Comment != "") {
			return nil, fmt.Errorf("syntax error: unexpected element value")
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("syntax error: empty element list")
	}
	return out, nil
}

func parseRawElement(t []token) (rawElement, error) {
	var e rawElement
	var keyToks, valToks []token
	inValue := false
	for i := 0; i < len(t); i++ {
		x := t[i]
		switch {
		case x.is("comment") && !inValue && i+1 < len(t) && t[i+1].kind == tString:
			e.Comment = t[i+1].val
			i++
		case x.is(":") && !inValue:
			inValue = true
			valToks = []token{}
		case inValue:
			valToks = append(valToks, x)
		default:
			keyToks = append(keyToks, x)
		}
	}
	e.Key = splitTop(keyToks, ".")
	if inValue {
		if len(valToks) == 0 {
			return e, fmt.Errorf("syntax error: missing element value")
		}
		e.Value = splitTop(valToks, ".")
	}
	for _, f := range append(append([][]token{}, e.Key...), e.Value...) {
		if len(f) == 0 {
			return e, fmt.Errorf("syntax error: empty element field")
		}
	}
	return e, nil
}

// splitTop splits on the separator word outside braces.
func splitTop(t []token, sep string) [][]token {
	var out [][]token
	var cur []token
	depth := 0
	for _, x := range t {
		switch {
		case x.is("{"):
			depth++
		case x.is("}"):
			depth--
		case depth == 0 && x.is(sep):
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, x)
	}
	return append(out, cur)
}

// parseDuration parses nft time strings: "10800s", "3h", "1d2h3m4s", "500ms".
func parseDuration(s string) (time.Duration, error) {
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("bad time %q", s)
		}
		n, _ := strconv.ParseInt(rest[:i], 10, 64)
		rest = rest[i:]
		j := 0
		for j < len(rest) && (rest[j] < '0' || rest[j] > '9') {
			j++
		}
		var unit time.Duration
		switch rest[:j] {
		case "d":
			unit = 24 * time.Hour
		case "h":
			unit = time.Hour
		case "m":
			unit = time.Minute
		case "s", "":
			unit = time.Second
		case "ms":
			unit = time.Millisecond
		default:
			return 0, fmt.Errorf("bad time %q", s)
		}
		total += time.Duration(n) * unit
		rest = rest[j:]
	}
	return total, nil
}

func joinToks(t []token) string {
	parts := make([]string, len(t))
	for i, x := range t {
		parts[i] = x.String()
	}
	return strings.Join(parts, " ")
}
