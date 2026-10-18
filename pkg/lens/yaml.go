package lens

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeYAML parses every document of a YAML stream into the value model.
func decodeYAML(data []byte, l *losses) ([]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []any
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		v, err := fromYAMLNode(&n, l)
		if err != nil {
			return nil, err
		}
		docs = append(docs, v)
	}
	return docs, nil
}

func fromYAMLNode(n *yaml.Node, l *losses) (any, error) {
	if n.HeadComment != "" || n.LineComment != "" || n.FootComment != "" {
		l.add("yaml comments dropped")
	}
	if n.Anchor != "" {
		l.add("yaml anchors/aliases expanded")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return fromYAMLNode(n.Content[0], l)
	case yaml.AliasNode:
		l.add("yaml anchors/aliases expanded")
		return fromYAMLNode(n.Alias, l)
	case yaml.SequenceNode:
		arr := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromYAMLNode(c, l)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case yaml.MappingNode:
		obj := newObject()
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, vn := n.Content[i], n.Content[i+1]
			if k.HeadComment != "" || k.LineComment != "" || k.FootComment != "" {
				l.add("yaml comments dropped")
			}
			if k.Kind != yaml.ScalarNode {
				return nil, errors.New("yaml: non-scalar mapping key")
			}
			if k.ShortTag() == "!!merge" {
				l.add("yaml merge key kept as a literal \"<<\" key")
			} else if k.ShortTag() != "!!str" {
				l.add("yaml non-string key %q converted to a string", k.Value)
			}
			v, err := fromYAMLNode(vn, l)
			if err != nil {
				return nil, err
			}
			if obj.set(k.Value, v) {
				l.add("duplicate key %q: last value kept", k.Value)
			}
		}
		return obj, nil
	case yaml.ScalarNode:
		return yamlScalar(n, l)
	}
	return nil, errors.New("yaml: unsupported node")
}

func yamlScalar(n *yaml.Node, l *losses) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, err
		}
		return b, nil
	case "!!int":
		if isJSONNumber(n.Value) {
			return json.Number(n.Value), nil
		}
		var i int64
		if err := n.Decode(&i); err == nil {
			return json.Number(strconv.FormatInt(i, 10)), nil
		}
		var u uint64
		if err := n.Decode(&u); err != nil {
			return nil, err
		}
		return json.Number(strconv.FormatUint(u, 10)), nil
	case "!!float":
		if isJSONNumber(n.Value) {
			return json.Number(n.Value), nil
		}
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, err
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			l.add("yaml %s has no JSON number form: kept as a string", n.Value)
			return n.Value, nil
		}
		return json.Number(formatFloat(f)), nil
	case "!!str":
		return n.Value, nil
	default:
		l.add("yaml %s scalar %q converted to a string", n.ShortTag(), n.Value)
		return n.Value, nil
	}
}

func isJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false
	}
	_, ok := v.(float64)
	return ok
}

func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// encodeYAML renders one document with 2-space indentation.
func encodeYAML(w *bytes.Buffer, v any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(toYAMLNode(v)); err != nil {
		return err
	}
	return enc.Close()
}

var yaml11Special = map[string]bool{
	"y": true, "n": true, "yes": true, "no": true, "on": true, "off": true, "~": true,
}

func yamlString(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	switch {
	case strings.Contains(s, "\n"):
		n.Style = yaml.LiteralStyle
	case yaml11Special[strings.ToLower(s)]:
		// yaml.v3 (YAML 1.2) leaves these plain, but a YAML 1.1 reader would
		// load them as booleans or null.
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

func toYAMLNode(v any) *yaml.Node {
	switch t := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(t), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(t)}
	case string:
		return yamlString(t)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range t {
			n.Content = append(n.Content, toYAMLNode(e))
		}
		if len(t) == 0 {
			n.Style = yaml.FlowStyle
		}
		return n
	case *Object:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range t.Keys {
			n.Content = append(n.Content,
				yamlString(k),
				toYAMLNode(t.Vals[k]))
		}
		if len(t.Keys) == 0 {
			n.Style = yaml.FlowStyle
		}
		return n
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: compactJSON(v)}
}
