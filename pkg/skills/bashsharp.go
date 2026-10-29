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

var decoratorLine = regexp.MustCompile(`^\s*@(require|ensure|guard|effects)\b`)

func bashSharpDecorators(body string) string {
	var lines strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if decoratorLine.MatchString(line) {
			lines.WriteString(line)
		}
	}
	return lines.String()
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
	// One identity, one contract reader, one capability key (pkg/unit): the
	// receipt names the exact body that ran and the promise it was keeping.
	id := unit.BodyIdentity([]byte(body))
	contract, _ := unit.ReadContract(body)
	capKey, _ := unit.CapabilityKey(contract)
	contracted := contract.Contracted()
	rec := AttestRecord{
		At: time.Now().UTC(), Name: name, Tier: p.tier, ContextKey: p.ctxKey, Status: &code,
		Capability: capKey,
		Attest:     dhntskills.Attestation{Skill: id, Valid: code == 0 && contracted},
		Attestation: unit.Attestation{Target: name, Identity: id, Capability: capKey, Valid: code == 0 && contracted,
			Effects: contract.EffectCap(), Uncontracted: !contracted},
	}
	switch {
	case code != 0:
		rec.Attest.Failed = []string{fmt.Sprintf("Bash# exit %d", code)}
	case !contracted:
		// Exit 0 is not a verdict: with no @require/@ensure there is nothing
		// the run could have been checked against, so it is never verified.
		// Like an uncontracted dag target, it leaves no receipt: a receipt is
		// evidence about a promise, and this unit made none.
		fmt.Fprintf(log, "skills: %s: uncontracted Bash# unit — ran, not verified\n", name)
	}
	if contracted {
		storeAttest(cfg, rec, log)
	}
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
	if ranClean(rec) || !adapt {
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
		candidate := unit.BashSharpBody(reply)
		if candidate == "" || bashSharpDecorators(candidate) != bashSharpDecorators(p.body) {
			lastErr = fmt.Errorf("repair rejected: missing Bash# fence or changed decorator lines")
			continue
		}
		trial, output := executeBashSharp(cfg, name, candidate, p, dir, log)
		if !ranClean(trial) {
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
