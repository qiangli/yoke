package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	dhntskills "github.com/dhnt/dhnt/skills"
	"github.com/qiangli/yoke/pkg/unit"
)

// BashSharpRunner executes a unit through the host's decorator-aware interpreter.
// Both output streams belong to the command log; stdout is reserved for receipts.
type BashSharpRunner func(ctx context.Context, dir string, env []string, body string, stdout, stderr io.Writer) (int, error)

// WithBashSharpRunner supplies the host interpreter without coupling yoke to bashy.
func WithBashSharpRunner(run BashSharpRunner) Option {
	return func(c *config) { c.bashSharpRunner = run }
}

// Preserve body bytes, including newlines, and ignore examples nested in other
// fenced blocks. An unterminated fence is not an executable unit.
func bashSharpFences(document string) string {
	var body strings.Builder
	var fence string
	var selected bool
	for _, line := range strings.SplitAfter(document, "\n") {
		trim := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trim, fence) && strings.Trim(trim, string(fence[0])) == "" {
				fence, selected = "", false
			} else if selected {
				body.WriteString(line)
			}
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			n := 0
			for n < len(trim) && trim[n] == trim[0] {
				n++
			}
			fence = trim[:n]
			tag := strings.TrimSpace(trim[n:])
			selected = tag == "bsh" || tag == "bashsharp"
		}
	}
	if fence != "" && selected {
		return ""
	}
	return body.String()
}

func bashSharpIdentity(body string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
}

var decoratorLine = regexp.MustCompile(`^\s*@(require|ensure|guard|effects)\b`)
var guardEffects = regexp.MustCompile(`@guard\s*\(\s*effects\s*:\s*["']([^"']*)["']`)

func bashSharpDecorators(body string) string {
	var lines strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if decoratorLine.MatchString(line) {
			lines.WriteString(line)
		}
	}
	return lines.String()
}

func bashSharpCap(body string) []string {
	var cap []string
	seen := map[string]bool{}
	for _, match := range guardEffects.FindAllStringSubmatch(body, -1) {
		for _, atom := range strings.Split(match[1], ",") {
			atom = strings.TrimSpace(atom)
			if atom != "" && !seen[atom] {
				cap = append(cap, atom)
				seen[atom] = true
			}
		}
	}
	return cap
}

// Overlay keys include the authored identity AND coordinate: a repair proven
// here must not silently replace a changed upstream unit or another context.
func bashSharpOverlay(cfg *config, p runPrep) string {
	key := sha256.Sum256([]byte(p.rootID + "\n" + p.ctxKey))
	return filepath.Join(cfg.cfgDir, "bashsharp", fmt.Sprintf("%x.bsh", key))
}

func executeBashSharp(cfg *config, name, body string, p runPrep, dir string, log io.Writer) (AttestRecord, string) {
	var output bytes.Buffer
	sink := io.MultiWriter(log, &output)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	code, err := cfg.bashSharpRunner(ctx, dir, os.Environ(), body, sink, sink)
	if err != nil {
		fmt.Fprintln(sink, err)
		if code == 0 {
			code = 1
		}
	}
	identity := bashSharpIdentity(body)
	rec := AttestRecord{
		At: time.Now().UTC(), Name: name, Tier: p.tier, ContextKey: p.ctxKey, Status: &code,
		Attest:      dhntskills.Attestation{Skill: identity, Valid: code == 0},
		Attestation: unit.Attestation{Target: name, Identity: identity, Valid: code == 0, Effects: bashSharpCap(body)},
	}
	if code != 0 {
		rec.Attest.Failed = []string{fmt.Sprintf("Bash# exit %d", code)}
	}
	storeAttest(cfg, rec, log)
	return rec, output.String()
}

func runBashSharp(cfg *config, name string, p runPrep, dir string, log io.Writer, complete Completer, attempts int, adapt bool) (AttestRecord, AdaptOutcome, error) {
	body, outcome := p.body, OutcomeBaseline
	overlay := bashSharpOverlay(cfg, p)
	if cfg.cfgDir != "" {
		learned, err := os.ReadFile(overlay)
		if err == nil && bashSharpDecorators(string(learned)) == bashSharpDecorators(p.body) {
			body, outcome = string(learned), OutcomeOverlay
			fmt.Fprintf(log, "skills: %s: running the host's learned version (overlay of %s)\n", name, shortID(p.rootID))
		}
	}
	rec, failure := executeBashSharp(cfg, name, body, p, dir, log)
	if rec.Valid || !adapt {
		return rec, outcome, nil
	}
	if complete == nil {
		return rec, OutcomeFailed, fmt.Errorf("skills: %q failed and no --repair-agent is configured", name)
	}
	if attempts <= 0 {
		attempts = 2
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		prompt := fmt.Sprintf("Repair this Bash# skill. Return the complete replacement executable unit in a bsh fence. Change ONLY steps; every @require/@ensure/@guard/@effects line must remain byte-identical, in order.\nSKILL.md:\n%s\nExecuted body:\n%s\nFailure (exit %d):\n%s", p.document, body, *rec.Status, failure)
		reply, err := complete(prompt)
		if err != nil {
			lastErr = err
			continue
		}
		candidate := bashSharpFences(reply)
		if candidate == "" || bashSharpDecorators(candidate) != bashSharpDecorators(p.body) {
			lastErr = fmt.Errorf("repair rejected: missing Bash# fence or changed decorator lines")
			continue
		}
		trial, output := executeBashSharp(cfg, name, candidate, p, dir, log)
		if !trial.Valid {
			failure = output
			lastErr = fmt.Errorf("candidate failed with exit %d", *trial.Status)
			continue
		}
		if cfg.cfgDir == "" {
			return trial, OutcomeFailed, fmt.Errorf("skills: no host-local store directory configured")
		}
		if err := os.MkdirAll(filepath.Dir(overlay), 0o755); err != nil {
			return trial, OutcomeFailed, err
		}
		// Rename an entire file so concurrent readers never see a partial fix.
		f, err := os.CreateTemp(filepath.Dir(overlay), ".repair-*")
		if err != nil {
			return trial, OutcomeFailed, err
		}
		temp := f.Name()
		_, err = f.WriteString(candidate)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temp, overlay)
		}
		os.Remove(temp)
		if err != nil {
			return trial, OutcomeFailed, err
		}
		fmt.Fprintf(log, "skills: %s: fix saved to host overlay\n", name)
		return trial, OutcomeRepaired, nil
	}
	return rec, OutcomeFailed, fmt.Errorf("skills: %q repair failed after %d attempts: %v", name, attempts, lastErr)
}
