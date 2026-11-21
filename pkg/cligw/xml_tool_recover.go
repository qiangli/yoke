package cligw

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

type parsedXMLInvoke struct {
	name   string
	params []parsedXMLParam
}

type parsedXMLParam struct {
	name  string
	value string
}

// recoverXMLToolCalls converts Anthropic-style terminal XML tool invocations
// (<function_calls><invoke name="...">...</invoke></function_calls> or bare
// <invoke name="...">...</invoke>) emitted as assistant text into the
// gateway's canonical {"tool_calls":[...]} JSON envelope.
//
// Acceptance is intentionally strict so ordinary prose or HTML/XML examples
// are never executed:
//   - the XML block must open the reply or open its own line after prose, and
//     must terminate the reply (only trailing whitespace may follow it);
//   - every <invoke> must name an offered tool permitted by tool_choice;
//   - only <parameter name="..."> elements are allowed inside <invoke>, with
//     typed JSON conversion and schema validation against Parameters.
func recoverXMLToolCalls(answer string, tools []openai.Tool, toolChoice json.RawMessage) (string, bool) {
	if len(tools) == 0 || toolChoiceIsNone(toolChoice) {
		return "", false
	}
	candidate, ok := extractXMLToolCandidate(answer)
	if !ok {
		return "", false
	}
	invokes, ok := parseXMLInvokes(candidate)
	if !ok || len(invokes) == 0 {
		return "", false
	}

	type envelopeCall struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	calls := make([]envelopeCall, 0, len(invokes))
	for _, inv := range invokes {
		if !toolChoicePermits(toolChoice, inv.name) {
			return "", false
		}
		offered, found := findOfferedTool(tools, inv.name)
		if !found {
			return "", false
		}
		args, valid := buildXMLToolArguments(inv.params, offered.Function.Parameters)
		if !valid {
			return "", false
		}
		calls = append(calls, envelopeCall{Name: offered.Function.Name, Arguments: args})
	}
	encoded, err := json.Marshal(struct {
		ToolCalls []envelopeCall `json:"tool_calls"`
	}{ToolCalls: calls})
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func findOfferedTool(tools []openai.Tool, name string) (openai.Tool, bool) {
	for _, offered := range tools {
		if offered.Type == "function" && offered.Function.Name == name {
			return offered, true
		}
	}
	return openai.Tool{}, false
}

func toolChoiceIsNone(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var mode string
	return json.Unmarshal([]byte(trimmed), &mode) == nil && mode == "none"
}

func toolChoicePermits(raw json.RawMessage, toolName string) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return true
	}
	var mode string
	if json.Unmarshal([]byte(trimmed), &mode) == nil {
		return mode != "none"
	}
	var chosen struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal([]byte(trimmed), &chosen) == nil && chosen.Type == "function" && chosen.Function.Name != "" {
		return chosen.Function.Name == toolName
	}
	return true
}

func extractXMLToolCandidate(answer string) (string, bool) {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return "", false
	}
	if unwrapped, ok := unwrapXMLFence(trimmed); ok {
		trimmed = unwrapped
	}
	if hasXMLToolStart(trimmed) {
		return trimmed, true
	}
	for offset := 0; offset < len(trimmed); {
		line := trimmed[offset:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		leftTrimmed := strings.TrimLeft(line, " \t\r")
		startOffset := offset + len(line) - len(leftTrimmed)
		if hasXMLToolStart(leftTrimmed) {
			return strings.TrimSpace(trimmed[startOffset:]), true
		}
		if strings.HasPrefix(leftTrimmed, "```") {
			if unwrapped, ok := unwrapXMLFence(strings.TrimSpace(trimmed[startOffset:])); ok && hasXMLToolStart(unwrapped) {
				return unwrapped, true
			}
		}
		offset += len(line) + 1
	}
	return "", false
}

func unwrapXMLFence(s string) (string, bool) {
	if !strings.HasPrefix(s, "```") {
		return "", false
	}
	lineEnd := strings.IndexByte(s, '\n')
	if lineEnd < 0 {
		return "", false
	}
	lang := strings.TrimSpace(s[3:lineEnd])
	if lang != "" && lang != "xml" {
		return "", false
	}
	rest := s[lineEnd+1:]
	if !strings.HasSuffix(rest, "```") {
		return "", false
	}
	inner := strings.TrimSpace(rest[:len(rest)-3])
	// Reject if another fence existed inside (more than one fenced block).
	if strings.Contains(inner, "```") {
		return "", false
	}
	return inner, true
}

func hasXMLToolStart(s string) bool {
	for _, prefix := range []string{"<function_calls", "<antml:function_calls", "<invoke", "<antml:invoke"} {
		if strings.HasPrefix(s, prefix) {
			if len(s) == len(prefix) {
				return true
			}
			next := s[len(prefix)]
			if next == '>' || next == ' ' || next == '\t' || next == '\n' || next == '\r' || next == '/' {
				return true
			}
		}
	}
	return false
}

func parseXMLInvokes(s string) ([]parsedXMLInvoke, bool) {
	rest := strings.TrimSpace(s)
	if ns, ok := matchXMLOpenTag(rest, "function_calls"); ok {
		tagEnd, attrs, selfClosing, ok := scanXMLOpenTag(rest)
		if !ok || selfClosing || len(attrs) != 0 {
			return nil, false
		}
		inner := rest[tagEnd:]
		closeTag := "</" + ns + "function_calls>"
		closeIdx := strings.Index(inner, closeTag)
		if closeIdx < 0 {
			return nil, false
		}
		if strings.TrimSpace(inner[closeIdx+len(closeTag):]) != "" {
			return nil, false
		}
		return parseInvokeSequence(inner[:closeIdx])
	}
	return parseInvokeSequence(rest)
}

func parseInvokeSequence(s string) ([]parsedXMLInvoke, bool) {
	rest := strings.TrimSpace(s)
	if rest == "" {
		return nil, false
	}
	var out []parsedXMLInvoke
	for rest != "" {
		inv, next, ok := parseOneInvoke(rest)
		if !ok {
			return nil, false
		}
		out = append(out, inv)
		rest = strings.TrimSpace(next)
	}
	return out, len(out) > 0
}

func parseOneInvoke(s string) (parsedXMLInvoke, string, bool) {
	ns, ok := matchXMLOpenTag(s, "invoke")
	if !ok {
		return parsedXMLInvoke{}, "", false
	}
	tagEnd, attrs, selfClosing, ok := scanXMLOpenTag(s)
	if !ok {
		return parsedXMLInvoke{}, "", false
	}
	name, ok := attrs["name"]
	if !ok || strings.TrimSpace(name) == "" || len(attrs) != 1 {
		return parsedXMLInvoke{}, "", false
	}
	inv := parsedXMLInvoke{name: name}
	if selfClosing {
		return inv, s[tagEnd:], true
	}
	rest := strings.TrimLeft(s[tagEnd:], " \t\r\n")
	closeTag := "</" + ns + "invoke>"
	seenParams := make(map[string]bool)
	for !strings.HasPrefix(rest, closeTag) {
		pns, ok := matchXMLOpenTag(rest, "parameter")
		if !ok {
			return parsedXMLInvoke{}, "", false
		}
		ptagEnd, pattrs, pSelfClosing, ok := scanXMLOpenTag(rest)
		if !ok {
			return parsedXMLInvoke{}, "", false
		}
		pname, ok := pattrs["name"]
		if !ok || strings.TrimSpace(pname) == "" || len(pattrs) != 1 || seenParams[pname] {
			return parsedXMLInvoke{}, "", false
		}
		seenParams[pname] = true
		var pval string
		if pSelfClosing {
			rest = rest[ptagEnd:]
		} else {
			pCloseTag := "</" + pns + "parameter>"
			afterOpen := rest[ptagEnd:]
			endIdx := strings.Index(afterOpen, pCloseTag)
			if endIdx < 0 {
				return parsedXMLInvoke{}, "", false
			}
			pval = unescapeXMLEntities(afterOpen[:endIdx])
			rest = afterOpen[endIdx+len(pCloseTag):]
		}
		inv.params = append(inv.params, parsedXMLParam{name: pname, value: pval})
		rest = strings.TrimLeft(rest, " \t\r\n")
	}
	return inv, rest[len(closeTag):], true
}

func matchXMLOpenTag(s, local string) (ns string, ok bool) {
	for _, prefix := range []string{"", "antml:"} {
		full := "<" + prefix + local
		if strings.HasPrefix(s, full) {
			if len(s) == len(full) {
				return "", false
			}
			next := s[len(full)]
			if next == '>' || next == ' ' || next == '\t' || next == '\n' || next == '\r' || next == '/' {
				return prefix, true
			}
		}
	}
	return "", false
}

// scanXMLOpenTag parses `<tag attr="val" ...>` or `<tag .../>`, returning the
// byte offset immediately after `>`, the decoded attributes, and whether it
// self-closed.
func scanXMLOpenTag(s string) (end int, attrs map[string]string, selfClosing bool, ok bool) {
	if len(s) < 2 || s[0] != '<' {
		return 0, nil, false, false
	}
	// Skip tag name.
	i := 1
	for i < len(s) && !isXMLSpace(s[i]) && s[i] != '>' && s[i] != '/' {
		i++
	}
	attrs = make(map[string]string)
	for i < len(s) {
		for i < len(s) && isXMLSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return 0, nil, false, false
		}
		if s[i] == '>' {
			return i + 1, attrs, false, true
		}
		if s[i] == '/' {
			if i+1 < len(s) && s[i+1] == '>' {
				return i + 2, attrs, true, true
			}
			return 0, nil, false, false
		}
		keyStart := i
		for i < len(s) && s[i] != '=' && !isXMLSpace(s[i]) && s[i] != '>' && s[i] != '/' {
			i++
		}
		key := s[keyStart:i]
		if key == "" {
			return 0, nil, false, false
		}
		for i < len(s) && isXMLSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return 0, nil, false, false
		}
		i++
		for i < len(s) && isXMLSpace(s[i]) {
			i++
		}
		if i >= len(s) || (s[i] != '"' && s[i] != '\'') {
			return 0, nil, false, false
		}
		quote := s[i]
		i++
		valStart := i
		for i < len(s) && s[i] != quote {
			i++
		}
		if i >= len(s) {
			return 0, nil, false, false
		}
		val := unescapeXMLEntities(s[valStart:i])
		i++ // consume closing quote
		if _, dup := attrs[key]; dup {
			return 0, nil, false, false
		}
		attrs[key] = val
		// After an attribute value, require whitespace or tag close.
		if i < len(s) && !isXMLSpace(s[i]) && s[i] != '>' && s[i] != '/' {
			return 0, nil, false, false
		}
	}
	return 0, nil, false, false
}

func isXMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func unescapeXMLEntities(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '&' {
			switch {
			case strings.HasPrefix(s[i:], "&lt;"):
				b.WriteByte('<')
				i += 4
				continue
			case strings.HasPrefix(s[i:], "&gt;"):
				b.WriteByte('>')
				i += 4
				continue
			case strings.HasPrefix(s[i:], "&amp;"):
				b.WriteByte('&')
				i += 5
				continue
			case strings.HasPrefix(s[i:], "&quot;"):
				b.WriteByte('"')
				i += 6
				continue
			case strings.HasPrefix(s[i:], "&apos;"):
				b.WriteByte('\'')
				i += 6
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func buildXMLToolArguments(params []parsedXMLParam, parameters json.RawMessage) (map[string]json.RawMessage, bool) {
	args := make(map[string]json.RawMessage, len(params))
	trimmedSchema := bytes.TrimSpace(parameters)
	if len(trimmedSchema) == 0 || bytes.Equal(trimmedSchema, []byte("null")) {
		for _, p := range params {
			encoded, err := json.Marshal(p.value)
			if err != nil {
				return nil, false
			}
			args[p.name] = encoded
		}
		return args, true
	}
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required             []string        `json:"required"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if json.Unmarshal(trimmedSchema, &schema) != nil || (schema.Type != "" && schema.Type != "object") {
		return nil, false
	}
	disallowAdditional := string(bytes.TrimSpace(schema.AdditionalProperties)) == "false"
	for _, p := range params {
		prop, known := schema.Properties[p.name]
		if !known {
			if disallowAdditional {
				return nil, false
			}
			encoded, err := json.Marshal(p.value)
			if err != nil {
				return nil, false
			}
			args[p.name] = encoded
			continue
		}
		encoded, ok := encodeXMLParamValue(p.value, prop.Type)
		if !ok {
			return nil, false
		}
		args[p.name] = encoded
	}
	for _, req := range schema.Required {
		if _, ok := args[req]; !ok {
			return nil, false
		}
	}
	if len(schema.Properties) > 0 && !validToolArguments(args, parameters, false) {
		return nil, false
	}
	return args, true
}

func encodeXMLParamValue(raw, schemaType string) (json.RawMessage, bool) {
	switch schemaType {
	case "", "string":
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, false
		}
		return encoded, true
	case "integer":
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, false
		}
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), true
	case "number":
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, false
		}
		var f float64
		if json.Unmarshal([]byte(trimmed), &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return json.RawMessage(trimmed), true
	case "boolean":
		trimmed := strings.TrimSpace(raw)
		if trimmed != "true" && trimmed != "false" {
			return nil, false
		}
		return json.RawMessage(trimmed), true
	case "object":
		trimmed := strings.TrimSpace(raw)
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) != nil || obj == nil {
			return nil, false
		}
		return json.RawMessage(trimmed), true
	case "array":
		trimmed := strings.TrimSpace(raw)
		var arr []any
		if json.Unmarshal([]byte(trimmed), &arr) != nil || arr == nil {
			return nil, false
		}
		return json.RawMessage(trimmed), true
	case "null":
		if strings.TrimSpace(raw) != "null" {
			return nil, false
		}
		return json.RawMessage("null"), true
	default:
		return nil, false
	}
}
