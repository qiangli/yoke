package ask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// KindApproval marks a Request as an approval: a yes/no decision about an action,
// not a value typed by a human. It lives in the SAME store as value asks — one
// pending directory, one id space, one reaper — so there is no second queue to
// keep consistent. `bashy approve` is the verb over it.
const KindApproval = "approval"

// Verdicts a Decision can carry.
const (
	VerdictAllow = "allow"
	VerdictDeny  = "deny"
)

// Where an approval was raised.
const (
	SourceConfirm = "confirm" // a yielded @confirm (exit 6, input_required)
	SourceACP     = "acp"     // an ACP session/request_permission
	SourceChat    = "chat"    // a `chat grant` on a live session
)

// How a Decision was reached.
const (
	ViaHuman   = "human"
	ViaReview  = "review"
	ViaTimeout = "timeout" // nobody answered: denied, never guessed
	ViaChat    = "chat"
	ViaACP     = "acp" // answered by the ACP host a `bashy acp` server relayed it to
)

// DefaultApprovalTTL is how long a pending approval waits for a human. It is far
// longer than a value ask's couple of minutes because the requester of a yielded
// @confirm has already exited: the approval, not a waiting process, is what holds
// the question open until someone resumes it.
const DefaultApprovalTTL = 24 * time.Hour

// DecisionRetention is how long a decided approval stays listed (and an allow
// stays redeemable) before the reaper drops it. Room events are the durable
// audit; this is only the window in which the decision can still be used.
const DecisionRetention = time.Hour

// Approval says what is being decided. Like Request, it never carries a secret:
// argv is the operation's shape, exactly as @confirm already prints it.
type Approval struct {
	// Token is the plan hash — the first four bytes of SHA-256 over the NUL-joined
	// argv (PlanToken). An answer applies to the operation that hashes to it and
	// to no other, so a resume cannot be steered onto a different action.
	Token   string   `json:"token"`
	Source  string   `json:"source"`
	Argv    []string `json:"argv,omitempty"`
	Effects []string `json:"effects,omitempty"`
	Sprint  string   `json:"sprint,omitempty"`
	// Session names the live session the approval belongs to (a room member id
	// for chat/ACP), so `chat grant` can find and settle it.
	Session string `json:"session,omitempty"`
	// Options are the ACP permission choices on offer, kept so an answer can name
	// the exact option the agent presented.
	Options []ApprovalOption `json:"options,omitempty"`
	// Mode is the agent permission mode that raised it (ask | review).
	Mode string `json:"mode,omitempty"`
}

// ApprovalOption mirrors an ACP permission option without importing pkg/acp.
type ApprovalOption struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// Decision is the single answer to an approval. It is written once; the first
// writer wins and a second Decide fails.
type Decision struct {
	Verdict string `json:"verdict"`
	// Always makes an allow reusable for the same token until the retention
	// window ends, and maps to allow-always on the ACP / chat grant surfaces.
	Always   bool      `json:"always,omitempty"`
	By       string    `json:"by"`
	Via      string    `json:"via"`
	Reason   string    `json:"reason,omitempty"`
	OptionID string    `json:"option_id,omitempty"`
	At       time.Time `json:"at"`
}

// Allowed reports a positive decision.
func (d Decision) Allowed() bool { return d.Verdict == VerdictAllow }

// ApprovalSpec is what a producer supplies to raise an approval.
type ApprovalSpec struct {
	Source  string
	Argv    []string
	Effects []string
	Sprint  string
	Session string
	Options []ApprovalOption
	Mode    string
	Prompt  string
	// Token overrides the argv-derived token. Leave empty unless the producer
	// already owns one (an @confirm token must be passed through unchanged).
	Token string
	TTL   time.Duration
}

const decisionFile = "decision.json"
const consumedFile = "consumed"

// PlanToken derives the approval token from an operation's argv. It is
// byte-identical to @confirm's confirmToken (bashy/internal/agentos/confirm.go):
// that equality is what lets a yielded @confirm and its resume find the same
// record.
func PlanToken(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:4])
}

// Raise persists an approval and announces it on the room timeline.
//
// Raising is idempotent: a still-pending approval with the same source, token and
// session is returned as is. A resumed @confirm that re-yields therefore lists
// ONE approval, not one per attempt.
func Raise(spec ApprovalSpec) (Request, error) {
	if spec.Source == "" {
		return Request{}, errors.New("ask: an approval needs a source")
	}
	token := spec.Token
	if token == "" {
		token = PlanToken(spec.Argv)
	}
	if existing, ok := findPending(spec.Source, token, spec.Session, spec.Argv); ok {
		return existing, nil
	}
	id, err := newID()
	if err != nil {
		return Request{}, fmt.Errorf("ask: generating a request id: %w", err)
	}
	ttl := spec.TTL
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	sprint := spec.Sprint
	if sprint == "" {
		sprint = strings.TrimSpace(os.Getenv("BASHY_SPRINT"))
	}
	now := time.Now()
	r := Request{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Prompt:        sanitizePrompt(spec.Prompt),
		Name:          "approval",
		Created:       now,
		Expires:       now.Add(ttl),
		ValueExpires:  now.Add(ttl + DecisionRetention),
		Sink:          Sink{Kind: SinkFile, Detail: "no value: a yes/no decision"},
		Requester:     currentRequester(),
		Kind:          KindApproval,
		Approval: &Approval{
			Token:   token,
			Source:  spec.Source,
			Argv:    append([]string(nil), spec.Argv...),
			Effects: append([]string(nil), spec.Effects...),
			Sprint:  sprint,
			Session: spec.Session,
			Options: append([]ApprovalOption(nil), spec.Options...),
			Mode:    spec.Mode,
		},
	}
	// Reap on the way in, as ask does, so the store stays self-cleaning.
	_, _ = List()
	if err := save(r); err != nil {
		return Request{}, err
	}
	room.Record(room.EventApproval, r.Requester.Principal, approvalTarget(r), "pending", r.Approval.summary())
	return r, nil
}

func approvalTarget(r Request) string { return "approval:" + shortID(r.ID) }

// summary is the one line a notification or the inbox shows. Argv is rendered
// the way @confirm prints it, so the human recognises the same operation.
func (a Approval) summary() string {
	what := strings.Join(a.Argv, " ")
	if what == "" {
		what = "(no argv)"
	}
	s := fmt.Sprintf("%s %s: %s", a.Source, a.Token, what)
	if len(a.Effects) > 0 {
		s += " (effects: " + strings.Join(a.Effects, ",") + ")"
	}
	return s
}

// Summary renders a request for a list or notification.
func (r Request) ApprovalSummary() string {
	if r.Approval == nil {
		return ""
	}
	return r.Approval.summary()
}

func findPending(source, token, session string, argv []string) (Request, bool) {
	all, err := List()
	if err != nil {
		return Request{}, false
	}
	for _, r := range all {
		a := r.Approval
		if !r.IsApproval() || a == nil || a.Source != source || a.Token != token || a.Session != session || !slices.Equal(a.Argv, argv) {
			continue
		}
		if _, decided := LoadDecision(r.ID); !decided {
			return r, true
		}
	}
	return Request{}, false
}

// ListApprovals returns live approvals, newest first. Decided ones are included
// until the retention window ends; callers filter with Decided / LoadDecision.
func ListApprovals() ([]Request, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, r := range all {
		if r.IsApproval() {
			out = append(out, r)
		}
	}
	return out, nil
}

// PendingApprovals is the feed for the inbox human lane: approvals still waiting
// for a decision, oldest first so the longest-waiting item leads.
func PendingApprovals() ([]Request, error) {
	all, err := ListApprovals()
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, r := range all {
		if _, decided := LoadDecision(r.ID); !decided {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// LoadDecision reads the decision for a request, if one has been made.
func LoadDecision(id string) (Decision, bool) {
	b, err := os.ReadFile(filepath.Join(requestDir(id), decisionFile))
	if err != nil {
		return Decision{}, false
	}
	var d Decision
	if json.Unmarshal(b, &d) != nil || d.Verdict == "" {
		return Decision{}, false
	}
	return d, true
}

// ErrDecided is returned by Decide when the approval already has a decision.
var ErrDecided = errors.New("ask: this approval has already been decided")

// DecideOpts qualifies a decision.
type DecideOpts struct {
	// Token, when set, must equal the approval's token: the decider proves it saw
	// the same plan hash that was raised (hitl's report digest, as a check).
	Token string
	// Via defaults to ViaHuman.
	Via string
}

// Decide settles an approval exactly once.
//
// The decision file is published with a hard link, so the first decider wins and
// every later one gets ErrDecided — a CAS without a lock. A reader never sees a
// partial decision because the content is complete before it is linked.
func Decide(idOrPrefix string, d Decision, opt DecideOpts) (Request, error) {
	r, err := Find(idOrPrefix)
	if err != nil {
		return Request{}, err
	}
	if !r.IsApproval() || r.Approval == nil {
		return Request{}, fmt.Errorf("ask: %s is a value request, not an approval — use `bashy ask claim`", shortID(r.ID))
	}
	if opt.Token != "" && !strings.EqualFold(opt.Token, r.Approval.Token) {
		return Request{}, fmt.Errorf("ask: token %s does not match approval %s (token %s) — the plan changed",
			opt.Token, shortID(r.ID), r.Approval.Token)
	}
	if d.Verdict != VerdictAllow && d.Verdict != VerdictDeny {
		return Request{}, fmt.Errorf("ask: verdict must be allow or deny, got %q", d.Verdict)
	}
	if d.Verdict == VerdictDeny {
		d.Always = false
	}
	d.Via = firstNonEmpty(opt.Via, d.Via, ViaHuman)
	if d.By == "" {
		d.By = principalName()
	}
	d.At = time.Now()
	if err := writeDecision(requestDir(r.ID), d); err != nil {
		return Request{}, err
	}
	room.Record(room.EventApproval, d.By, approvalTarget(r), d.Verdict,
		fmt.Sprintf("%s via %s (token %s)", d.Verdict, d.Via, r.Approval.Token))
	return r, nil
}

func writeDecision(dir string, d Decision) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".decision-*")
	if err != nil {
		return fmt.Errorf("ask: recording the decision: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Link(tmpName, filepath.Join(dir, decisionFile)); err != nil {
		if os.IsExist(err) {
			return ErrDecided
		}
		return fmt.Errorf("ask: recording the decision: %w", err)
	}
	return nil
}

// WaitDecision blocks until the approval is decided, the context ends, or the
// request disappears (cancelled or reaped).
func WaitDecision(ctx context.Context, id string) (Decision, error) {
	const poll = 100 * time.Millisecond
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if d, ok := LoadDecision(id); ok {
			return d, nil
		}
		if _, err := os.Stat(filepath.Join(requestDir(id), requestFile)); err != nil {
			return Decision{}, fmt.Errorf("ask: approval %s is gone (cancelled or expired)", shortID(id))
		}
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-t.C:
		}
	}
}

// DenyUnanswered records a timeout denial — "deny when no human", never "allow
// because nobody objected". It tolerates losing the race to a real decision and
// returns whichever decision actually stands.
func DenyUnanswered(id, reason string) Decision {
	_, err := Decide(id, Decision{Verdict: VerdictDeny, By: "bashy", Reason: reason}, DecideOpts{Via: ViaTimeout})
	if err != nil && !errors.Is(err, ErrDecided) {
		return Decision{Verdict: VerdictDeny, By: "bashy", Via: ViaTimeout, Reason: reason, At: time.Now()}
	}
	d, _ := LoadDecision(id)
	return d
}

// Resolve is the resume side of a yielded @confirm. The token is only a short
// hash, so session and exact argv must also match before a decision is used.
//
// An allow without Always is single-use — the first Resolve consumes it, so one
// "yes" cannot authorise the same operation twice. A deny is reported every time
// until it ages out, so a refused operation stays refused on replay. Only decided
// approvals raised by source are considered; the newest wins.
func Resolve(source, token, session string, argv []string) (Decision, bool) {
	all, err := ListApprovals()
	if err != nil {
		return Decision{}, false
	}
	for _, r := range all { // newest first
		if r.Approval == nil || r.Approval.Source != source || r.Approval.Token != token || r.Approval.Session != session || !slices.Equal(r.Approval.Argv, argv) {
			continue
		}
		d, ok := LoadDecision(r.ID)
		if !ok {
			continue
		}
		if d.Allowed() && !d.Always && !consume(r.ID) {
			continue // already redeemed: a once-allow is gone
		}
		room.Record(room.EventApproval, d.By, approvalTarget(r), "resume", fmt.Sprintf("%s resumed (token %s)", d.Verdict, token))
		return d, true
	}
	return Decision{}, false
}

// consume latches an approval redeemed; false means someone redeemed it first.
func consume(id string) bool {
	f, err := os.OpenFile(filepath.Join(requestDir(id), consumedFile),
		os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func approvalExpired(r Request, now time.Time) bool {
	if d, ok := LoadDecision(r.ID); ok {
		return now.After(d.At.Add(DecisionRetention))
	}
	// No pid check, unlike a value ask: the requester of a yielded @confirm has
	// exited by design, and its question must survive it.
	return now.After(r.Expires)
}

// ReviewFunc is an automated reviewer — an LLM reached through the model door in
// production, a stub in tests. It returns a definite verdict or an error; any
// other outcome is treated as "could not decide".
type ReviewFunc func(ctx context.Context, r Request) (verdict, reason string, err error)

// DefaultReviewTimeout bounds a reviewer. A reviewer that cannot answer quickly
// is no reviewer at all, and the approval must reach a human instead.
const DefaultReviewTimeout = 30 * time.Second

// Review runs the reviewer under a bound and records its verdict when it gives a
// definite one. It reports decided=false — leaving the approval pending for a
// human — on a reviewer error, a timeout, an unrecognised verdict, or no
// reviewer: review mode degrades to ask, it never fails open.
func Review(ctx context.Context, r Request, rv ReviewFunc, timeout time.Duration) (Decision, bool) {
	if rv == nil || !r.IsApproval() {
		return Decision{}, false
	}
	if timeout <= 0 {
		timeout = DefaultReviewTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		verdict, reason string
		err             error
	}
	ch := make(chan result, 1)
	go func() {
		v, why, err := rv(rctx, r)
		ch <- result{v, why, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-rctx.Done():
		return Decision{}, false
	}
	verdict := strings.ToLower(strings.TrimSpace(res.verdict))
	if res.err != nil || (verdict != VerdictAllow && verdict != VerdictDeny) {
		return Decision{}, false
	}
	// A reviewer's allow is never "always": standing grants are a human's call.
	_, err := Decide(r.ID, Decision{Verdict: verdict, By: "reviewer", Reason: res.reason}, DecideOpts{Via: ViaReview})
	if err != nil && !errors.Is(err, ErrDecided) {
		return Decision{}, false
	}
	d, ok := LoadDecision(r.ID)
	return d, ok
}

// PendingForSession lists undecided approvals raised for a live session.
func PendingForSession(session string) ([]Request, error) {
	pending, err := PendingApprovals()
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, r := range pending {
		if r.Approval != nil && r.Approval.Session == session {
			out = append(out, r)
		}
	}
	return out, nil
}

// Grant is `chat grant`'s side of the shared record. If the session has pending
// approvals it settles them (the same record an ACP request or @confirm raised);
// otherwise it writes a decided record so the grant is still on the books. It
// returns the approvals it settled or created.
func Grant(session, actor string, always bool, argv []string) ([]Request, error) {
	pending, err := PendingApprovals()
	if err != nil {
		return nil, err
	}
	var settled []Request
	for _, r := range pending {
		if r.Approval == nil || r.Approval.Session != session {
			continue
		}
		d := Decision{Verdict: VerdictAllow, Always: always, By: actor}
		if opt := optionFor(r.Approval.Options, always); opt != "" {
			d.OptionID = opt
		}
		if _, err := Decide(r.ID, d, DecideOpts{Via: ViaChat}); err != nil && !errors.Is(err, ErrDecided) {
			return settled, err
		}
		settled = append(settled, r)
	}
	if len(settled) > 0 {
		return settled, nil
	}
	r, err := Raise(ApprovalSpec{Source: SourceChat, Argv: argv, Session: session, Prompt: "chat grant " + session})
	if err != nil {
		return nil, err
	}
	if _, err := Decide(r.ID, Decision{Verdict: VerdictAllow, Always: always, By: actor}, DecideOpts{Via: ViaChat}); err != nil && !errors.Is(err, ErrDecided) {
		return nil, err
	}
	return []Request{r}, nil
}

// optionFor picks the allow option an ACP agent offered for the grant strength.
func optionFor(opts []ApprovalOption, always bool) string {
	want, fallback := "allow_once", "allow_always"
	if always {
		want, fallback = fallback, want
	}
	for _, kind := range []string{want, fallback} {
		for _, o := range opts {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
