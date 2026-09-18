package craft

// THE MODEL HALF OF ABSORPTION — prose in, a typed contract out.
//
// This is the only place in the read/write split where a model is genuinely
// required. Deciding what a paragraph of English GUARANTEES is not something
// pattern matching can do, and it is exactly the work worth paying for once:
// the result is a contract that every later run checks for free, forever.
//
// Two rules make that trade safe rather than reckless.
//
// PREMIUM ONLY. A weak model that produces a plausible-but-wrong contract does
// not merely fail — it poisons the store with a promise nobody verified, and
// every later reader inherits it. An under-banded normalisation is worse than
// none, so the gate REFUSES rather than degrading.
//
// TRANSPILABILITY IS THE FLOOR, NOT THE CEILING. dhnt's validity rule catches
// garbage: a reply that does not parse to the typed AST is rejected outright,
// loudly. What it cannot catch is a reply that parses perfectly and means
// something else. That residual risk is why normalised skills land quarantined
// until they attest, and why the original prose is retained — a bad
// decomposition must stay diagnosable and re-runnable, never a lossy one-way
// transform.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// MinNormaliseBand is the lowest model band permitted to decompose prose.
//
// Band 3 is the "chooses method from a contract" tier. Below it, models in this
// fleet have measurably failed to decompose reliably — the recorded case is a
// conductor run with 9.4x tool-call repetition that never converged. Cheap to
// enforce, expensive to discover after a store has been filled.
const MinNormaliseBand = 3

// ErrUnderBanded reports a normalisation attempt below MinNormaliseBand.
var ErrUnderBanded = fmt.Errorf("craft: normalisation needs a band-%d or better model", MinNormaliseBand)

// normaliseTimeout bounds one decomposition. Generous: this is write-path work
// that happens once per skill, not a hot path.
const normaliseTimeout = 5 * time.Minute

// ExecCompleter runs a headless agent CLI as a dhnt Completer, appending the
// prompt as the final argument (`claude -p <prompt>`, `codex exec <prompt>`).
//
// Mirrors skills.execCompleter deliberately: same invocation convention, so an
// operator who knows how to point --repair-agent at a tool already knows how to
// point --normalise-agent at one.
func ExecCompleter(command string) dhntskills.Completer {
	return func(prompt string) (string, error) {
		argv := strings.Fields(command)
		if len(argv) == 0 {
			return "", fmt.Errorf("craft: empty normalise-agent command")
		}
		ctx, cancel := context.WithTimeout(context.Background(), normaliseTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], prompt)...)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("craft: normalise agent: %w", err)
		}
		return string(out), nil
	}
}

// NormaliserOptions configures NewNormaliser.
type NormaliserOptions struct {
	// Band is the capability band of the model behind Complete. Zero means
	// unknown, which is REFUSED — an unverified band is not a passing band.
	Band int
	// Lang is the source natural language of the prose (default "en"). The
	// canonical form is language-neutral, so this affects extraction only.
	Lang string
	// Retries bounds re-asking after an unparsable reply.
	Retries int
}

// NewNormaliser builds the absorption hook from a completer.
//
// Returns an error rather than a degraded normaliser when the band is unknown
// or too low: silently proceeding with a weak model is the failure this guards.
func NewNormaliser(complete dhntskills.Completer, opts NormaliserOptions) (Normaliser, error) {
	if complete == nil {
		return nil, fmt.Errorf("craft: no completer supplied")
	}
	if opts.Band < MinNormaliseBand {
		return nil, fmt.Errorf("%w (got band %d; declare the model's band or use a stronger one)", ErrUnderBanded, opts.Band)
	}
	lang := strings.TrimSpace(opts.Lang)
	if lang == "" {
		lang = "en"
	}
	retries := opts.Retries
	if retries <= 0 {
		retries = 2
	}

	glossary, err := dhntskills.SeedGlossary()
	if err != nil {
		return nil, fmt.Errorf("craft: loading the dhnt glossary: %w", err)
	}

	return func(c Candidate) (dhntskills.Skill, error) {
		prose := strings.TrimSpace(c.Body)
		if prose == "" {
			prose = strings.TrimSpace(c.Description)
		}
		if prose == "" {
			return dhntskills.Skill{}, fmt.Errorf("candidate %q has no prose to normalise", c.Name)
		}
		sk, _, err := dhntskills.Normalise(prose, glossary, lang, complete, retries)
		if err != nil {
			return dhntskills.Skill{}, err
		}
		// A normalisation that produced no contract has not answered the only
		// question worth asking of it. Reporting that plainly keeps the
		// candidate quarantined instead of admitting a skill that guarantees
		// nothing under a name that implies it does.
		if len(sk.Contract) == 0 {
			return dhntskills.Skill{}, fmt.Errorf("normalisation of %q produced no contract", c.Name)
		}
		return sk, nil
	}, nil
}
