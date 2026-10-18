package lens

import "strings"

// Skill to script (Sprint 446 S22, first slice). A skill document carries
// its runnable body in one fenced block (```bsh, ```bash or ```sh); the
// skill→script row extracts that body, normalizing line endings to LF. It never runs anything,
// never splits the body into an argv, and never calls a model: output is the
// fence body verbatim. A document with no shell fence, an unterminated fence,
// an empty body, or more than one shell fence is ExitInvalid — ambiguity is
// refused, never guessed.
//
// The existing skill argv rows are untouched: skill→bash and friends still
// parse the single fenced command into an argv.
func init() {
	aliases["script"] = "script"
	argvOnly["script"] = true // a script body needs no detection; it is a row target
	// Markdown prose and other fences are discarded, so this is a lossy row.
	rows = append(rows, Row{From: "skill", To: "script", Fidelity: Lossy, conv: skillToScript})
}

// skillScriptLangs are the fence info words whose bodies are shell script,
// the same set the skill argv rows parse.
var skillScriptLangs = map[string]bool{"bsh": true, "bash": true, "sh": true, "posix": true}

// ExtractSkillScript extracts the single shell fence body of a skill document
// (Sprint 446 S22). It normalizes line endings to LF, never executes anything,
// never splits into argv, and never calls a model.
// A document with no shell fence, an unterminated fence, an empty body, or
// more than one shell fence is ExitInvalid — ambiguity is refused fail-closed.
func ExtractSkillScript(data []byte) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var script []string
	shells := 0
	for i := 0; i < len(lines); i++ {
		fence, info := fenceOpen(lines[i])
		if fence == "" {
			continue
		}
		body := []string(nil)
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimLeft(lines[j], " "), fence) && strings.Trim(strings.TrimLeft(lines[j], " "), "`") == "" {
				i, closed = j, true
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			return nil, errorf(ExitInvalid, "invalid skill input: unterminated %s fence", fence)
		}
		if !skillScriptLangs[info] {
			continue
		}
		shells++
		script = body
	}
	switch {
	case shells == 0:
		return nil, errorf(ExitInvalid, "invalid skill input: no ```bsh fence")
	case shells > 1:
		return nil, errorf(ExitInvalid, "invalid skill input: want exactly one shell fence, got %d", shells)
	}
	if strings.TrimSpace(strings.Join(script, "\n")) == "" {
		return nil, errorf(ExitInvalid, "invalid skill input: empty script body")
	}
	return []byte(strings.Join(script, "\n") + "\n"), nil
}

// CompileSkillText is an alias for ExtractSkillScript.
func CompileSkillText(data []byte) ([]byte, error) {
	return ExtractSkillScript(data)
}

// skillToScript extracts the single shell fence body of a skill document.
func skillToScript(data []byte, _ *losses) ([]byte, error) {
	return ExtractSkillScript(data)
}
