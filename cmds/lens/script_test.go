package lenscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/lens"
)

// TestScriptCLI tests lens -f skill -t script via the command runner.
func TestScriptCLI(t *testing.T) {
	dir := t.TempDir()
	doc := "# Skill Document\n\nRuns a simple pure echo command.\n\n```bsh\necho 'hello from lens'\n```\n\nFooter prose.\n"
	want := "echo 'hello from lens'\n"

	// 1. Convert via stdin.
	out, errb, code := runTool(t, dir, doc, "-f", "skill", "-t", "script")
	if code != 0 || errb != "" || out != want {
		t.Fatalf("stdin skill -> script: code=%d out=%q err=%q", code, out, errb)
	}

	// 2. Convert via file argument.
	skillFile := filepath.Join(dir, "task.skill")
	if err := os.WriteFile(skillFile, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errb, code = runTool(t, dir, "", "-f", "skill", "-t", "script", "task.skill")
	if code != 0 || errb != "" || out != want {
		t.Fatalf("file skill -> script: code=%d out=%q err=%q", code, out, errb)
	}

	// 3. Determinism: repeated run produces identical bytes.
	out2, errb2, code2 := runTool(t, dir, doc, "-f", "skill", "-t", "script")
	if code2 != 0 || errb2 != "" || out2 != out {
		t.Fatalf("determinism mismatch: code=%d out=%q", code2, out2)
	}

	// 4. Rejection of input with no shell fence: fails with ExitInvalid.
	out, errb, code = runTool(t, dir, "# Prose only\n\nNo code here.\n", "-f", "skill", "-t", "script")
	if code != lens.ExitInvalid || !strings.Contains(errb, "no ```bsh fence") {
		t.Fatalf("no fence: code=%d out=%q err=%q", code, out, errb)
	}

	// 5. Rejection of multiple shell fences: fails with ExitInvalid.
	multiDoc := "```bsh\necho one\n```\n\n```bash\necho two\n```\n"
	out, errb, code = runTool(t, dir, multiDoc, "-f", "skill", "-t", "script")
	if code != lens.ExitInvalid || !strings.Contains(errb, "exactly one shell fence") {
		t.Fatalf("multiple fences: code=%d out=%q err=%q", code, out, errb)
	}

	// 6. Rejection of empty script body: fails with ExitInvalid.
	emptyDoc := "```bsh\n   \n```\n"
	out, errb, code = runTool(t, dir, emptyDoc, "-f", "skill", "-t", "script")
	if code != lens.ExitInvalid || !strings.Contains(errb, "empty script body") {
		t.Fatalf("empty body: code=%d out=%q err=%q", code, out, errb)
	}

	// 7. Unsupported argv knobs fail with ExitUsage.
	out, errb, code = runTool(t, dir, doc, "-f", "skill", "-t", "script", "--switch=slash")
	if code != lens.ExitUsage {
		t.Fatalf("switch flag on script row: code=%d err=%q", code, errb)
	}
}
