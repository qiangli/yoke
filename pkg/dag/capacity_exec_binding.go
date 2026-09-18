package dag

import (
	"bytes"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func capacityInventoryAllowed(allowed, requested []CapacityExecutable) bool {
	if len(requested) > 8 || len(allowed) > 8 {
		return false
	}
	known := map[string]string{}
	for _, item := range allowed {
		path := filepath.Clean(item.Path)
		if !filepath.IsAbs(path) || known[path] != "" {
			return false
		}
		if b, e := hex.DecodeString(item.SHA256); e != nil || len(b) != 32 {
			return false
		}
		known[path] = strings.ToLower(item.SHA256)
	}
	seen := map[string]bool{}
	for _, item := range requested {
		path := filepath.Clean(item.Path)
		if seen[path] || !filepath.IsAbs(path) || len(item.SHA256) != 64 || known[path] != strings.ToLower(item.SHA256) {
			return false
		}
		seen[path] = true
	}
	return true
}
func capacitySameInventory(a, b []CapacityExecutable) bool {
	return len(a) == len(b) && capacityInventoryAllowed(a, b)
}

// Static direct executable calls are bound to declared absolute paths before
// interpretation, so PATH cannot silently select a different compiler. A tool's
// own descendants, libraries and package resolution are outside this proof.
func capacityPinnedBody(body string, inventory []CapacityExecutable) (string, error) {
	if capacityBoundedBuiltinBody(body) {
		return body, nil
	}
	if len(inventory) == 0 {
		return "", errors.New("external task requires declared executable fingerprints")
	}
	paths := map[string]string{}
	for _, item := range inventory {
		path := filepath.Clean(item.Path)
		if !filepath.IsAbs(path) {
			return "", errors.New("executable path must be absolute")
		}
		base := filepath.Base(path)
		if prior := paths[base]; prior != "" && prior != path {
			return "", errors.New("ambiguous executable basename")
		}
		paths[base] = path
		paths[path] = path
	}
	program, e := syntax.NewParser().Parse(strings.NewReader(body), "")
	if e != nil {
		return "", e
	}
	var refused error
	syntax.Walk(program, func(node syntax.Node) bool {
		if refused != nil {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := call.Args[0].Lit()
		switch name {
		case "printf", "echo", "true", "false", ":", "cd", "pwd", "test", "[", "export", "unset", "readonly", "declare":
			return true
		}
		path := paths[name]
		if path == "" {
			refused = errors.New("dynamic or undeclared direct executable call")
			return false
		}
		call.Args[0] = &syntax.Word{Parts: []syntax.WordPart{&syntax.SglQuoted{Value: path}}}
		return true
	})
	if refused != nil {
		return "", refused
	}
	var out bytes.Buffer
	if e = syntax.NewPrinter().Print(&out, program); e != nil {
		return "", e
	}
	return out.String(), nil
}
