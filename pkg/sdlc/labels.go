package sdlc

import "strings"

// Label-driven control surface. Two reserved families drive the loop:
//
//	sdlc:*    — the PRIVATE lifecycle state machine (intake → in-progress → qa →
//	            approved → done). Applied by the conductor; also an idempotency
//	            guard (an in-progress issue is never picked up twice) and a
//	            human-visible status board on the issue.
//	deploy:*  — the PUBLIC deploy baton. Adding deploy:<env> is what triggers the
//	            deploy GitHub Action; the conductor applies it after a green gate
//	            (or a human applies it to promote). It is the seam across the
//	            private-control-plane / public-deploy boundary — no runner or
//	            secret crosses it, only a label.
//
// See bashy/docs/local-loom-sdlc-control-plane.md.
const (
	LabelInitiate   = "sdlc:go"          // human blesses an issue → conductor picks it up
	LabelInProgress = "sdlc:in-progress" // conductor claimed it (dedup guard)
	LabelQA         = "sdlc:qa"          // awaiting / in QA review
	LabelApproved   = "sdlc:approved"    // approved, ready to deploy
	LabelBlocked    = "sdlc:blocked"     // do not pick up
	LabelIgnore     = "sdlc:ignore"      // never pick up
	LabelDone       = "sdlc:done"        // resolved + deployed

	// DeployLabelPrefix + <env> (deploy:dev|deploy:qa|deploy:prod) is the deploy
	// trigger. The suffix names the target environment.
	DeployLabelPrefix = "deploy:"
)

// reservedSkipLabels: an issue carrying any of these is NOT eligible for a fresh
// pickup — it is blocked, already in flight, or past the intake stage.
var reservedSkipLabels = map[string]bool{
	LabelIgnore:     true,
	LabelBlocked:    true,
	LabelInProgress: true,
	LabelQA:         true,
	LabelApproved:   true,
	LabelDone:       true,
}

// ReservedLabels returns the full vocabulary an app should bootstrap once
// (`gh label create ...` / `bashy sdlc init`) so the loop can apply them.
func ReservedLabels() []string {
	return []string{
		LabelInitiate, LabelInProgress, LabelQA, LabelApproved, LabelBlocked, LabelIgnore, LabelDone,
		DeployLabelForEnv("dev"), DeployLabelForEnv("qa"), DeployLabelForEnv("prod"),
		"priority:p0", "priority:p1", "priority:p2", "priority:p3",
		"type:bug", "type:enhancement", "type:task", "type:docs", "type:chore",
	}
}

func normLabel(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// eligibleByLabels reports whether an issue with these label names (and title) is
// eligible for a fresh SDLC pickup: non-empty title and no reserved skip label.
// When requireInitiate is true the issue must also carry LabelInitiate (sdlc:go)
// — the recommended gate for public GitHub intake, so only human-blessed issues
// start a round. Loom/local intake typically passes false ("a plain open issue
// is enough").
func eligibleByLabels(names []string, title string, requireInitiate bool) bool {
	if strings.TrimSpace(title) == "" {
		return false
	}
	hasInitiate := false
	for _, n := range names {
		ln := normLabel(n)
		if reservedSkipLabels[ln] {
			return false
		}
		if ln == LabelInitiate {
			hasInitiate = true
		}
	}
	return !requireInitiate || hasInitiate
}

// priorityByLabels maps priority:pN → scheduling order (p0 highest = 0), default 100.
func priorityByLabels(names []string) int {
	score := 100
	for _, n := range names {
		switch normLabel(n) {
		case "priority:p0":
			return 0
		case "priority:p1":
			score = min(score, 1)
		case "priority:p2":
			score = min(score, 2)
		case "priority:p3":
			score = min(score, 3)
		}
	}
	return score
}

// IsControlLabel reports whether a label is a reserved control label (sdlc:* or
// deploy:*) — the owner-driven labels the GitHub→loom mirror propagates so the
// owner can manage the loop from GitHub.
func IsControlLabel(name string) bool {
	n := normLabel(name)
	return strings.HasPrefix(n, "sdlc:") || strings.HasPrefix(n, DeployLabelPrefix)
}

// DeployLabelForEnv returns the deploy baton label for an environment, e.g.
// DeployLabelForEnv("qa") == "deploy:qa".
func DeployLabelForEnv(env string) string { return DeployLabelPrefix + normLabel(env) }

// EnvFromDeployLabel parses a deploy label back to its env ("deploy:qa" → "qa");
// returns "" when the label is not a deploy label.
func EnvFromDeployLabel(label string) string {
	ln := normLabel(label)
	if rest, ok := strings.CutPrefix(ln, DeployLabelPrefix); ok {
		return rest
	}
	return ""
}
