package lens

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// TestSkillScript pins the skill→script slice (Sprint 446 S22): one fenced
// bsh/bash/sh body comes out byte for byte — no execution, no argv split,
// no model call — and anything ambiguous is refused.
func TestSkillScript(t *testing.T) {
	// A multi-line script no argv row could carry: pipes, a loop, expansions
	// and quotes must all survive literally.
	body := "# deploy\nset -e\ngit status --short | head -5\necho \"dollar $HOME and $(whoami) stay literal\"\nfor i in 1 2; do echo $i; done\n"
	for _, fence := range []string{"bsh", "bash", "sh"} {
		doc := "# Skill\n\nProse before.\n\n```json\n{\"not\": \"script\"}\n```\n\n```" + fence + "\n" + body + "```\n\nProse after.\n"
		res := convert(t, "skill", "script", []byte(doc))
		if string(res.Data) != body {
			t.Fatalf("%s: got %q want %q", fence, res.Data, body)
		}
		if res.Row.Fidelity != Lossy || res.Row.Via() != "go" || res.Row.IsArgv() || !res.Row.Installed() {
			t.Fatalf("%s: row %+v", fence, res.Row)
		}
		if len(res.Losses) != 0 || res.Executable != "" {
			t.Fatalf("%s: losses=%v executable=%q", fence, res.Losses, res.Executable)
		}
	}

	// Indented fences and CRLF still extract the body bytes.
	res := convert(t, "skill", "script", []byte("text\n  ```sh\n  ls -l 'a b'\n  ```\n"))
	if string(res.Data) != "  ls -l 'a b'\n" {
		t.Fatalf("indented: %q", res.Data)
	}
	res = convert(t, "skill", "script", []byte("```bsh\r\necho hi\r\n```\r\n"))
	if string(res.Data) != "echo hi\n" {
		t.Fatalf("crlf: %q", res.Data)
	}

	// The row is routed and listed; the format needs -f/-t, never detection.
	if Canonical("script") != "script" {
		t.Fatal("Canonical(script)")
	}
	if r, err := Route("skill", "script"); err != nil || r.From != "skill" || r.To != "script" {
		t.Fatalf("route: %v %v", r, err)
	}
	found := false
	for _, r := range Rows() {
		if r.From == "skill" && r.To == "script" {
			found = true
		}
	}
	if !found {
		t.Fatal("skill -> script missing from Rows")
	}
	if f := Detect("run.sh", []byte("echo hi\n")); f != "" {
		t.Fatalf("run.sh detected as %s: scripts need -f", f)
	}
	if f := Detect("x.script", []byte("echo hi\n")); f != "" {
		t.Fatalf("x.script detected as %s", f)
	}

	// Ambiguous, missing and degenerate inputs are ExitInvalid, not guesses.
	for _, c := range []struct{ in, msg string }{
		{"no fence here\n", "no ```bsh fence"},
		{"```json\n{}\n```\n", "no ```bsh fence"},
		{"```bsh\necho hi\n", "unterminated"},
		{"```bsh\n```\n", "empty script body"},
		{"```bsh\n\n```\n", "empty script body"},
		{"```bsh\necho a\n```\n\n```bash\necho b\n```\n", "exactly one shell fence"},
		{"```sh\necho a\n```\n```bsh\necho b\n```\n", "exactly one shell fence"},
		{"````bash\necho a\n````\n\n```bsh\necho b\n```\n", "exactly one shell fence"},
	} {
		_, err := Convert(context.Background(), Request{From: "skill", To: "script", Data: []byte(c.in)})
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: %v (want %q)", c.in, err, c.msg)
		}
		wantExit(t, err, ExitInvalid)
	}

	// The agent knobs belong to argv rows, not to script extraction.
	_, err := Convert(context.Background(), Request{From: "skill", To: "script", Data: []byte("```bsh\necho hi\n```\n"), Argv: ArgvOptions{Switch: "slash"}})
	wantExit(t, err, ExitUsage)

	// The existing argv skill rows are preserved: a rendered skill still
	// parses back to its argv, and the argv table is unchanged.
	argv := []string{"bashy", "kb", "search", "lens rows"}
	rendered := mustRender(t, "skill", argv)
	back, err := parseArgv("skill", rendered, &losses{})
	if err != nil || !slices.Equal(back, argv) {
		t.Fatalf("skill argv round trip: %q %v", back, err)
	}
	r, err := Route("cmd", "skill")
	if err != nil || !r.IsArgv() {
		t.Fatalf("cmd -> skill must stay an argv row: %v", err)
	}
	n := 0
	for _, r := range Rows() {
		if r.IsArgv() {
			n++
		}
	}
	if n != 10*9-2 {
		t.Fatalf("%d argv rows", n)
	}

	// Deterministic under the gated flags' repeat call.
	doc := []byte("```bash\necho deterministic\n```\n")
	first, err := Convert(context.Background(), Request{From: "skill", To: "script", Data: doc})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Convert(context.Background(), Request{From: "skill", To: "script", Data: doc})
	if err != nil || !bytes.Equal(first.Data, second.Data) {
		t.Fatal("skill -> script is not deterministic")
	}
}
