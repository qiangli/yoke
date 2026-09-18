package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type unknownPathError struct {
	noun string
	path string
}

func (e *unknownPathError) Error() string {
	return fmt.Sprintf("fleet: unknown %s path %q; valid paths are listed above", e.noun, e.path)
}

func splitAssignment(t reflect.Type, s string) (string, string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] != '=' {
			continue
		}
		path := s[:i]
		if _, ok := parsePath(t, path); ok {
			return path, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("fleet: --set needs <dotted.path>=<value>, got %q", s)
}

func editPaths(noun string, record any, sets, unsets []string) error {
	data, err := Marshal(record)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	rootType := nounType(noun)
	for _, assignment := range sets {
		path, value, err := splitAssignment(rootType, assignment)
		if err != nil {
			if candidate, _, ok := strings.Cut(assignment, "="); ok && candidate != "" {
				return &unknownPathError{noun: noun, path: candidate}
			}
			return err
		}
		parts, _ := parsePath(rootType, path)
		target, ok := pathType(rootType, parts)
		if !ok {
			return &unknownPathError{noun: noun, path: path}
		}
		valueNode, err := valueForType(value, target)
		if err != nil {
			return fmt.Errorf("fleet: --set %s: %w", path, err)
		}
		if err := setNode(doc.Content[0], rootType, parts, valueNode); err != nil {
			return fmt.Errorf("fleet: --set %s: %w", path, err)
		}
	}
	for _, path := range unsets {
		parts, ok := parsePath(rootType, path)
		if !ok {
			return &unknownPathError{noun: noun, path: path}
		}
		if err := unsetNode(doc.Content[0], rootType, parts); err != nil {
			return fmt.Errorf("fleet: --unset %s: %w", path, err)
		}
	}
	return strictNodeDecode(&doc, record)
}

func parsePath(t reflect.Type, path string) ([]string, bool) {
	if path == "" {
		return nil, false
	}
	t = indirectType(t)
	switch t.Kind() {
	case reflect.Struct:
		head, tail, hasTail := strings.Cut(path, ".")
		f, ok := structField(t, head)
		if !ok {
			return nil, false
		}
		if !hasTail {
			return []string{head}, true
		}
		rest, ok := parsePath(f.Type, tail)
		return append([]string{head}, rest...), ok
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, false
		}
		elem := indirectType(t.Elem())
		if elem.Kind() != reflect.Struct && elem.Kind() != reflect.Map && elem.Kind() != reflect.Slice {
			return []string{path}, true
		}
		return splitDynamicPath(t.Elem(), path, func(s string) bool { return s != "" })
	case reflect.Slice:
		return splitDynamicPath(t.Elem(), path, listSegment)
	default:
		return nil, false
	}
}

func splitDynamicPath(elem reflect.Type, path string, validHead func(string) bool) ([]string, bool) {
	for i := 0; i < len(path); i++ {
		if path[i] != '.' {
			continue
		}
		head := path[:i]
		if !validHead(head) {
			continue
		}
		if rest, ok := parsePath(elem, path[i+1:]); ok {
			return append([]string{head}, rest...), true
		}
	}
	if validHead(path) {
		return []string{path}, true
	}
	return nil, false
}

func pathType(t reflect.Type, parts []string) (reflect.Type, bool) {
	if len(parts) == 0 {
		return t, true
	}
	t = indirectType(t)
	switch t.Kind() {
	case reflect.Struct:
		f, ok := structField(t, parts[0])
		if !ok {
			return nil, false
		}
		return pathType(f.Type, parts[1:])
	case reflect.Map:
		if t.Key().Kind() != reflect.String || parts[0] == "" {
			return nil, false
		}
		return pathType(t.Elem(), parts[1:])
	case reflect.Slice:
		if !listSegment(parts[0]) {
			return nil, false
		}
		return pathType(t.Elem(), parts[1:])
	default:
		return nil, false
	}
}

func structField(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if yamlName(f) == name && name != "-" {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

func listSegment(s string) bool {
	if strings.HasPrefix(s, "name=") && len(s) > len("name=") {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0
}

func valueForType(value string, t reflect.Type) (*yaml.Node, error) {
	t = indirectType(t)
	var parsed any
	switch t.Kind() {
	case reflect.String:
		parsed = value
	case reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("want bool: %w", err)
		}
		parsed = v
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(value, 10, t.Bits())
		if err != nil {
			return nil, fmt.Errorf("want int: %w", err)
		}
		parsed = v
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v, err := strconv.ParseUint(value, 10, t.Bits())
		if err != nil {
			return nil, fmt.Errorf("want uint: %w", err)
		}
		parsed = v
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(value, t.Bits())
		if err != nil {
			return nil, fmt.Errorf("want float: %w", err)
		}
		parsed = v
	default:
		if err := yaml.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, fmt.Errorf("want %s: %w", schemaType(t), err)
		}
	}
	var n yaml.Node
	if err := n.Encode(parsed); err != nil {
		return nil, err
	}
	return &n, nil
}

func strictNodeDecode(doc *yaml.Node, dst any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc.Content[0]); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("fleet: edited record destination must be a pointer")
	}
	fresh := reflect.New(rv.Elem().Type())
	if err := dec.Decode(fresh.Interface()); err != nil {
		return fmt.Errorf("fleet: edited record is invalid: %w", err)
	}
	rv.Elem().Set(fresh.Elem())
	return nil
}

func setNode(node *yaml.Node, t reflect.Type, parts []string, value *yaml.Node) error {
	if len(parts) == 0 {
		*node = *value
		return nil
	}
	t = indirectType(t)
	switch t.Kind() {
	case reflect.Struct:
		f, _ := structField(t, parts[0])
		child := mappingValue(node, parts[0])
		if child == nil {
			child = newContainer(f.Type)
			appendMapping(node, parts[0], child)
		}
		return setNode(child, f.Type, parts[1:], value)
	case reflect.Map:
		child := mappingValue(node, parts[0])
		if child == nil {
			child = newContainer(t.Elem())
			appendMapping(node, parts[0], child)
		}
		return setNode(child, t.Elem(), parts[1:], value)
	case reflect.Slice:
		child, err := sequenceValue(node, t.Elem(), parts[0], true)
		if err != nil {
			return err
		}
		return setNode(child, t.Elem(), parts[1:], value)
	default:
		return fmt.Errorf("cannot descend through %s", schemaType(t))
	}
}

func unsetNode(node *yaml.Node, t reflect.Type, parts []string) error {
	if len(parts) == 0 {
		return nil
	}
	t = indirectType(t)
	if len(parts) == 1 {
		switch t.Kind() {
		case reflect.Struct, reflect.Map:
			removeMapping(node, parts[0])
			return nil
		case reflect.Slice:
			return removeSequence(node, t.Elem(), parts[0])
		}
	}
	switch t.Kind() {
	case reflect.Struct:
		f, _ := structField(t, parts[0])
		child := mappingValue(node, parts[0])
		if child == nil {
			return nil
		}
		return unsetNode(child, f.Type, parts[1:])
	case reflect.Map:
		child := mappingValue(node, parts[0])
		if child == nil {
			return nil
		}
		return unsetNode(child, t.Elem(), parts[1:])
	case reflect.Slice:
		child, err := sequenceValue(node, t.Elem(), parts[0], false)
		if err != nil || child == nil {
			return err
		}
		return unsetNode(child, t.Elem(), parts[1:])
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func appendMapping(node *yaml.Node, key string, value *yaml.Node) {
	if node.Kind == 0 || node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		node.Kind, node.Tag, node.Value = yaml.MappingNode, "!!map", ""
		node.Content = nil
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func removeMapping(node *yaml.Node, key string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

func newContainer(t reflect.Type) *yaml.Node {
	t = indirectType(t)
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case reflect.Slice:
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
}

func sequenceValue(node *yaml.Node, elem reflect.Type, segment string, create bool) (*yaml.Node, error) {
	if node.Kind == 0 || node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		node.Kind, node.Tag, node.Value = yaml.SequenceNode, "!!seq", ""
		node.Content = nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected list")
	}
	if strings.HasPrefix(segment, "name=") {
		want := strings.TrimPrefix(segment, "name=")
		identity := identityField(elem)
		for _, item := range node.Content {
			if got := mappingValue(item, identity); got != nil && got.Value == want {
				return item, nil
			}
		}
		if !create {
			return nil, nil
		}
		item := newContainer(elem)
		appendMapping(item, identity, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: want})
		node.Content = append(node.Content, item)
		return item, nil
	}
	index, _ := strconv.Atoi(segment)
	if index > len(node.Content) {
		return nil, fmt.Errorf("list index %d is past length %d", index, len(node.Content))
	}
	if index == len(node.Content) {
		if !create {
			return nil, nil
		}
		node.Content = append(node.Content, newContainer(elem))
	}
	return node.Content[index], nil
}

func removeSequence(node *yaml.Node, elem reflect.Type, segment string) error {
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	if strings.HasPrefix(segment, "name=") {
		want := strings.TrimPrefix(segment, "name=")
		identity := identityField(elem)
		for i, item := range node.Content {
			if got := mappingValue(item, identity); got != nil && got.Value == want {
				node.Content = append(node.Content[:i], node.Content[i+1:]...)
				return nil
			}
		}
		return nil
	}
	index, _ := strconv.Atoi(segment)
	if index >= len(node.Content) {
		return nil
	}
	node.Content = append(node.Content[:index], node.Content[index+1:]...)
	return nil
}

func identityField(t reflect.Type) string {
	if name, ok := namedIdentityField(t); ok {
		return name
	}
	return "name"
}

func namedIdentityField(t reflect.Type) (string, bool) {
	t = indirectType(t)
	for _, name := range []string{"name", "version", "host"} {
		if _, ok := structField(t, name); ok {
			return name, true
		}
	}
	return "", false
}

func fieldNode(record any, noun, path string) (*yaml.Node, error) {
	parts, ok := parsePath(nounType(noun), path)
	if !ok {
		return nil, &unknownPathError{noun: noun, path: path}
	}
	data, err := Marshal(record)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	node := doc.Content[0]
	t := nounType(noun)
	for _, part := range parts {
		t = indirectType(t)
		switch t.Kind() {
		case reflect.Struct:
			f, _ := structField(t, part)
			node = mappingValue(node, part)
			t = f.Type
		case reflect.Map:
			node = mappingValue(node, part)
			t = t.Elem()
		case reflect.Slice:
			var err error
			node, err = sequenceValue(node, t.Elem(), part, false)
			if err != nil {
				return nil, err
			}
			t = t.Elem()
		}
		if node == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
		}
	}
	return node, nil
}

func emitField(w io.Writer, record any, noun, path string, asJSON bool) error {
	node, err := fieldNode(record, noun, path)
	if err != nil {
		return err
	}
	if asJSON {
		var value any
		if err := node.Decode(&value); err != nil {
			return err
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(append(data, '\n'))
		return err
	}
	if node.Kind == yaml.ScalarNode {
		_, err := fmt.Fprintln(w, node.Value)
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}
