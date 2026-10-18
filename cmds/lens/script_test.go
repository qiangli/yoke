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

// TestScriptCLI_ExplainAndDryRun tests lens -f skill -t script with --explain,
// --dry-run, --json, and --cap.
func TestScriptCLI_ExplainAndDryRun(t *testing.T) {
	dir := t.TempDir()
	doc := "# Skill Document\n\nRuns a pure echo command.\n\n```bsh\necho 'hello from lens'\n```\n\nFooter prose.\n"

	// 1. --explain prints zero-execution diagnostic view and writes no files.
	out, errb, code := runTool(t, dir, doc, "-f", "skill", "-t", "script", "--explain")
	if code != 0 || errb != "" {
		t.Fatalf("--explain: code=%d err=%q", code, errb)
	}
	for _, expected := range []string{"artifact: ", "digest: sha256:", "required_caps: read-only", "effects: pure", "execution: zero-execution dry-run"} {
		if !strings.Contains(out, expected) {
			t.Errorf("--explain output missing %q: %q", expected, out)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("--explain wrote %d files, expected 0", len(entries))
	}

	// 2. --dry-run behaves identically with zero execution and no side effects.
	outDry, errbDry, codeDry := runTool(t, dir, doc, "-f", "skill", "-t", "script", "--dry-run")
	if codeDry != 0 || errbDry != "" || outDry != out {
		t.Fatalf("--dry-run: code=%d err=%q out=%q", codeDry, errbDry, outDry)
	}

	// 3. --explain --json outputs structured JSON.
	outJSON, errbJSON, codeJSON := runTool(t, dir, doc, "-f", "skill", "-t", "script", "--explain", "--json")
	if codeJSON != 0 || errbJSON != "" {
		t.Fatalf("--explain --json: code=%d err=%q", codeJSON, errbJSON)
	}
	if !strings.Contains(outJSON, `"artifact"`) || !strings.Contains(outJSON, `"capabilities"`) {
		t.Fatalf("--explain --json output missing keys: %s", outJSON)
	}

	// 4. Denied caps via --cap fails closed before execution.
	docPwd := "```bsh\npwd\n```\n"
	_, errbCap, codeCap := runTool(t, dir, docPwd, "-f", "skill", "-t", "script", "--explain", "--cap=pure")
	if codeCap != lens.ExitInvalid || !strings.Contains(errbCap, `denied`) {
		t.Fatalf("--cap denied effect: code=%d err=%q", codeCap, errbCap)
	}

	// 5. Allowed caps via --cap succeeds.
	outAllowed, errbAllowed, codeAllowed := runTool(t, dir, docPwd, "-f", "skill", "-t", "script", "--explain", "--cap=read-only,read")
	if codeAllowed != 0 || errbAllowed != "" {
		t.Fatalf("--cap allowed: code=%d err=%q out=%q", codeAllowed, errbAllowed, outAllowed)
	}

	// 6. Rejected unsafe script under --explain fails closed without running or writing files.
	sentinel := filepath.Join(dir, "sentinel.txt")
	hostileDoc := "```bsh\necho evil > " + sentinel + "\n```\n"
	_, errbHostile, codeHostile := runTool(t, dir, hostileDoc, "-f", "skill", "-t", "script", "--explain")
	if codeHostile != lens.ExitInvalid || !strings.Contains(errbHostile, "redirection") {
		t.Fatalf("hostile --explain: code=%d err=%q", codeHostile, errbHostile)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("hostile script executed under --explain!")
	}

	// 7. --seat with --explain shows delegation seat line and writes no files.
	outSeat, errbSeat, codeSeat := runTool(t, dir, doc, "-f", "skill", "-t", "script", "--explain", "--seat=ci-worker-1")
	if codeSeat != 0 || errbSeat != "" {
		t.Fatalf("--seat: code=%d err=%q", codeSeat, errbSeat)
	}
	if !strings.Contains(outSeat, "delegation_seat: ci-worker-1 (help:script, no_input:true)") {
		t.Errorf("--seat output missing delegation_seat line: %q", outSeat)
	}

	// 8. --seat with --explain --json includes seat in JSON output.
	outSeatJSON, errbSeatJSON, codeSeatJSON := runTool(t, dir, doc, "-f", "skill", "-t", "script", "--explain", "--seat=ci-worker-1", "--json")
	if codeSeatJSON != 0 || errbSeatJSON != "" {
		t.Fatalf("--seat --json: code=%d err=%q", codeSeatJSON, errbSeatJSON)
	}
	if !strings.Contains(outSeatJSON, `"seat": "ci-worker-1"`) || !strings.Contains(outSeatJSON, `"seat_help": "script"`) || !strings.Contains(outSeatJSON, `"seat_no_input": true`) {
		t.Fatalf("--seat --json missing expected fields: %s", outSeatJSON)
	}

	// 9. --seat with denied --cap fails closed before any worker execution.
	_, errbSeatCap, codeSeatCap := runTool(t, dir, docPwd, "-f", "skill", "-t", "script", "--explain", "--seat=ci-worker-1", "--cap=pure")
	if codeSeatCap != lens.ExitInvalid || !strings.Contains(errbSeatCap, "denied") {
		t.Fatalf("--seat with denied --cap: code=%d err=%q", codeSeatCap, errbSeatCap)
	}
}
