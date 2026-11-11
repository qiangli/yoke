// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ref

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// SplitAppID checks the app:<command>/<id> contract. The item id belongs to
// the app; core only requires one path-safe segment and leaves its spelling
// untouched. In particular, an app may use a name instead of a Desk id.
func SplitAppID(value string) (command, id string, err error) {
	command, id, ok := strings.Cut(value, "/")
	if !ok || !validAppCommand(command) || id == "" || id == "." || id == ".." ||
		strings.ContainsAny(id, "/\\") || strings.IndexFunc(id, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", "", fmt.Errorf("%w: expected app:<command>/<id> with a lowercase command and one path-safe id", ErrInvalidAppID)
	}
	// Ref spellings may cross URL boundaries before reaching a runner. Refuse
	// encoded traversal too, while keeping ordinary opaque ids verbatim.
	decoded, err := url.PathUnescape(id)
	if err != nil || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\") {
		return "", "", fmt.Errorf("%w: app id contains an encoded path segment", ErrInvalidAppID)
	}
	return command, id, nil
}

func validAppCommand(command string) bool {
	if command == "" {
		return false
	}
	for _, r := range command {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				if r != '-' {
					return false
				}
			}
		}
	}
	return true
}

// AppRunner returns the output of <command> show <id> --json. The embedding
// shell supplies it so this leaf package never executes commands or bypasses
// an app's own access checks.
type AppRunner func(command, id string) ([]byte, error)

// AppResolver resolves only explicitly allowed app commands. Its command list
// is a snapshot of the registered app commands supplied by the embedding shell.
type AppResolver struct {
	commands map[string]bool
	runner   AppRunner
}

func NewAppResolver(commands []string, runner AppRunner) *AppResolver {
	r := &AppResolver{commands: make(map[string]bool, len(commands)), runner: runner}
	for _, command := range commands {
		if validAppCommand(command) {
			r.commands[command] = true
		}
	}
	return r
}

// Resolve implements Resolver. The app's show output establishes whether the
// item exists and whether this caller can read it; its errors remain intact.
func (r *AppResolver) Resolve(value string) (Node, error) {
	command, id, err := SplitAppID(value)
	if err != nil {
		return Node{}, err
	}
	if r == nil || !r.commands[command] {
		var allowed []string
		if r != nil {
			for name := range r.commands {
				allowed = append(allowed, name)
			}
		}
		sort.Strings(allowed)
		if len(allowed) == 0 {
			allowed = []string{"(none)"}
		}
		return Node{}, fmt.Errorf("app command %q: %w; allowed commands: %s", command, ErrNotFound, strings.Join(allowed, ", "))
	}
	if r.runner == nil {
		return Node{}, fmt.Errorf("app %s: no show runner wired", command)
	}
	output, err := r.runner(command, id)
	if err != nil {
		return Node{}, err
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(output, &item); err != nil || item == nil {
		return Node{}, fmt.Errorf("app %s show %s --json: invalid JSON object", command, id)
	}
	n := NewNode(App, value)
	for _, field := range []string{"title", "name"} {
		if raw, ok := item[field]; ok && json.Unmarshal(raw, &n.Title) == nil && n.Title != "" {
			break
		}
	}
	n.Open = "bashy " + command + " show " + id + " --json"
	return n, nil
}
