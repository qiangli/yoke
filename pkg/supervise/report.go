package supervise

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// redactHome rewrites the user's home dir to `~` in filed output — reports get
// committed and agent CLIs print absolute workdirs.
func redactHome(s string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	if home = strings.TrimRight(home, string(os.PathSeparator)); len(home) < 2 {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *Plan) save() error {
	dir, err := storeDir(p.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p.Schema = schemaVersion
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "plan.json"), b)
}

func findRepoRoot(dir string) string {
	d := dir
	for d != "" {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return ""
}

// reportPath: an explicit --out path wins; else <repo>/docs/supervise/, else the
// session store.
func (p *Plan) reportPath() string {
	out := strings.TrimSpace(p.Out)
	if out != "" && out != "docs" {
		return out
	}
	name := fmt.Sprintf("supervise-%s-%s.md", p.Created.Format("2006-01-02T15-04"), slug(firstWords(p.Goal, 6)))
	if root := findRepoRoot(p.Cwd); root != "" {
		return filepath.Join(root, "docs", "supervise", name)
	}
	dir, _ := storeDir(p.ID)
	return filepath.Join(dir, "report.md")
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

// writeReport files the supervision report.
func (p *Plan) writeReport(res *Result) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Supervision — %s\n", p.Goal)
	fmt.Fprintf(&b, "Session: `%s`  ·  fleet: %s", p.ID, strings.Join(p.Fleet, ", "))
	if strings.TrimSpace(p.Supervisor) != "" {
		fmt.Fprintf(&b, "  ·  optional supervisor: %s", p.Supervisor)
	}
	b.WriteString("\n\n")

	pass, fail, unver := 0, 0, 0
	for _, v := range res.Verdicts {
		if v.Unverified {
			unver++
		}
		if v.Passed {
			pass++
		} else {
			fail++
		}
	}
	verdict := "NOT CONVERGED"
	if res.Converged {
		verdict = "CONVERGED"
	}
	fmt.Fprintf(&b, "**%s** — %d/%d tasks passed", verdict, pass, len(p.Contracts))
	if fail > 0 {
		fmt.Fprintf(&b, ", %d failed", fail)
	}
	if unver > 0 {
		fmt.Fprintf(&b, ", %d unverified", unver)
	}
	b.WriteString("\n\n")

	if s := strings.TrimSpace(res.Judgment); s != "" {
		fmt.Fprintf(&b, "## Supervisor summary (optional)\n%s\n\n", redactHome(s))
	}

	b.WriteString("## Task outcomes (supplied gates are authoritative)\n\n")
	b.WriteString("| Task | Verdict | Worker | Attempts | Exit |\n|---|---|---|---:|---:|\n")
	for _, v := range res.Verdicts {
		st := "❌ fail"
		switch {
		case v.Passed && v.Unverified:
			st = "✅ pass (unverified)"
		case v.Passed:
			st = "✅ pass"
		case v.Unverified:
			st = "❌ fail (unverified)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d |\n", v.Contract, st, v.Worker, v.Attempts, v.GateExit)
	}

	// Failing tasks: show the gate tail so the report is actionable.
	for _, v := range res.Verdicts {
		if v.Passed || v.Unverified || strings.TrimSpace(v.Detail) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n### %s — gate output (exit %d)\n\n```\n%s\n```\n", v.Contract, v.GateExit, redactHome(v.Detail))
	}

	dir, _ := storeDir(p.ID)
	fmt.Fprintf(&b, "\nTranscript: `%s`\n", redactHome(filepath.Join(dir, "transcript.jsonl")))

	path := p.reportPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := atomicWrite(path, []byte(b.String())); err != nil {
		return "", err
	}
	return path, nil
}
