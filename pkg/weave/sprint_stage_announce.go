package weave

import (
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
)

// A SPRINT'S STAGE CHANGES REACHED NOBODY.
//
// Several managers run concurrently on one host against shared repos, a shared
// gate and a shared pin order, so a sprint being started, taken, handed off,
// stopped, ended, moved or aborted is information the others act on. Every one
// of those verbs changed state and told only the sprint's own thread.
//
// # Why this is a projection and not a post in each verb
//
// Sprinkling a post call into eight commands guarantees drift, and a stage verb
// added later would silently not announce. The sprint ALREADY has an
// append-only stage log and every entry passes through one function —
// weaveStoryAppend. So the announcement is a VIEW of that log: hook it once,
// and anything that records a transition is announced for free.
//
// # Why a kind and not a command name
//
// The filter is the thread entry's KIND, because that is what distinguishes a
// transition from a note, and it is what makes the announcement IDEMPOTENT
// where it matters. `sprint take` is also RECOVERY — its own help says a stale
// lease is taken directly after a SIGKILL or token exhaustion — and a flapping
// conductor takes repeatedly onto a board that deletes nothing. Re-taking a
// lease you already hold writes `kindProgress`, not `kindStage`, so a crash
// loop announces nothing while a real handover announces once.
//
// # The taxonomy, decided here
//
//	stage     a TRANSITION: created, moved, started, stopped, ended, taken from
//	          somebody else, handed off, aborted. These announce.
//	system    a state note that is not a transition, including resuming a lease
//	          you already hold.
//	decision  an operator judgement recorded on the thread.
//	progress  checkpoints.
//	note      everything else.
const (
	kindStage    = "stage"
	kindSystem   = "system"
	kindDecision = "decision"
	kindProgress = "progress"
)

// announceTopic scopes the post so a reader who declared this concern sees it
// uncapped, and the audience carries the same exemption by MEMBERSHIP: a
// seated manager is addressed without having declared anything. Everyone else
// sees it under the ordinary -n cap, which is the right default: these are
// useful to peers and noise to everyone else.
const announceTopic = "sprint"

// announceEnabled is the operator's off switch. Announcing is on by default —
// help text alone would be a success state reached by the absence of evidence,
// which is exactly what docs/fleet-evidence-invariant.md forbids.
var announceEnabled = true

// announceStageFn is the send, as a var so a test can observe what would go out
// without writing to the operator's real board.
var announceStageFn = postStageToBoard

// lastAnnounceErr carries a failed announcement out to the command wrapper.
//
// THE STATE CHANGE IS THE TRANSACTION. A take that reports failure because a
// MESSAGE did not send is worse than no message: the lease really was claimed,
// and an exit code saying otherwise makes an operator undo work that succeeded.
// So the error never propagates — it is reported, and reported VISIBLY, because
// an announcement that silently did not happen is the same defect one layer
// down.
var lastAnnounceErr error

// announceStage projects one stage entry onto the message board.
func announceStage(s *weaveStory, who, body string) {
	if !announceEnabled || s == nil {
		return
	}
	if v := strings.TrimSpace(os.Getenv("BASHY_SPRINT_ANNOUNCE")); v == "0" || strings.EqualFold(v, "off") {
		return
	}
	from := strings.TrimSpace(who)
	if from == "" {
		from = weaveConductorName("")
	}
	if err := announceStageFn(from, stageMessage(s, body)); err != nil {
		lastAnnounceErr = err
	}
}

// stageMessage is what peers actually need: which sprint, what happened, and a
// reference they can act on. Kept short because the board caps a body at 1024
// bytes and never truncates or splits one.
func stageMessage(s *weaveStory, body string) string {
	title := strings.TrimSpace(s.Title)
	if len(title) > 60 {
		title = title[:57] + "..."
	}
	msg := fmt.Sprintf("sprint #%d %s — %s", s.ID, title, strings.TrimSpace(body))
	if len(msg) > 1024 {
		msg = msg[:1021] + "..."
	}
	return msg
}

// announceRole is who a stage change is FOR: the other seated managers. It
// resolves at read time through the same host seam `mb send --role` uses, so
// a lease moving between agents re-addresses future reads with no rewrite.
const announceRole = "conductor"

func postStageToBoard(from, body string) error {
	_, err := bus.Send(bus.SendRequest{
		From:     from,
		Topic:    announceTopic,
		Body:     body,
		Audience: &bus.Audience{Role: announceRole},
	})
	return err
}

// reportAnnounceFailure surfaces a failed projection on stderr and clears it.
// Called by the command wrapper AFTER the state change has committed.
func reportAnnounceFailure(w interface{ Write([]byte) (int, error) }, op string) {
	if lastAnnounceErr == nil {
		return
	}
	fmt.Fprintf(w, "%s: the state change was recorded, but announcing it on the board failed: %v\n",
		op, lastAnnounceErr)
	lastAnnounceErr = nil
}
