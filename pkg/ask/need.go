package ask

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// SourceNeeds marks an approval raised as a typed "needs you" request: a
// question an agent or sprint manager puts to its human (Sprint 745). It shares
// the approval queue, id space and decision record with every other approval;
// what it adds is a kind, options, a recommendation, refs, a deadline, risk tags
// and a route (delegate first, or the human).
const SourceNeeds = "needs"

// Need kinds.
const (
	NeedDecision = "decision"
	NeedApproval = "approval"
	NeedAction   = "action"
	NeedInfo     = "info"
)

// NeedKinds is the closed set of kinds.
var NeedKinds = []string{NeedDecision, NeedApproval, NeedAction, NeedInfo}

// Urgency levels.
var NeedUrgencies = []string{"low", "normal", "urgent"}

// NonDelegable is the default set of risk tags only the human answers. The
// human may widen a delegation to cover some of them (Delegation.AllowRisk);
// nobody else can.
var NonDelegable = []string{"deploy", "security", "credentials", "money", "delete", "access"}

// Routes.
const (
	RouteDelegate = "delegate"
	RouteHuman    = "human"
)

// ViaDelegate marks a decision made by the human's delegate on their behalf;
// ViaBoard one made by the human on the sprint board web UI.
const (
	ViaDelegate = "delegate"
	ViaBoard    = "board"
)

// NeedTTL is how long a needs request waits. It is a question put to a person,
// not a resume token, so it outlives a day; NeedRetention keeps a decided one
// listed (the on-behalf audit) long after the requester continued.
const (
	NeedTTL       = 30 * 24 * time.Hour
	NeedRetention = 30 * 24 * time.Hour
)

// Need is the typed body of a needs request.
type Need struct {
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Recommend string    `json:"recommend,omitempty"`
	Refs      []string  `json:"refs,omitempty"`
	Deadline  time.Time `json:"deadline,omitzero"`
	Urgency   string    `json:"urgency"`
	Risk      []string  `json:"risk,omitempty"`
	Key       string    `json:"key,omitempty"`
	Human     string    `json:"human"`
	From      string    `json:"from,omitempty"`
	Route     string    `json:"route"`
	Delegate  string    `json:"delegate,omitempty"`
}

// NeedSpec is what a caller supplies to raise a needs request.
type NeedSpec struct {
	Kind      string
	Title     string
	Options   []ApprovalOption
	Recommend string
	Refs      []string
	Deadline  time.Time
	Urgency   string
	Risk      []string
	Key       string
	Sprint    string
}

// IsNeed reports whether this approval is a typed needs request.
func (r Request) IsNeed() bool {
	return r.IsApproval() && r.Approval != nil && r.Approval.Need != nil
}

// detectAgentTool is fleet.DetectTool, a seam so tests run as a human even
// under an agent harness.
var detectAgentTool = fleet.DetectTool

// callerAgent names the agent this process runs as, or "" for a human at a
// shell. An agent is a session that declared BASHY_AGENT or runs under an agent
// harness the fleet recognises.
func callerAgent() string {
	if a := strings.TrimSpace(os.Getenv("BASHY_AGENT")); a != "" {
		return a
	}
	if tool, ok := detectAgentTool(); ok && tool != "" {
		return tool
	}
	return ""
}

// RaiseNeed validates spec and raises a needs request, routed to the human's
// delegate when one covers it and to the human otherwise. Raising again with the
// same key while the first is undecided returns the first.
func RaiseNeed(spec NeedSpec) (Request, error) {
	if !slices.Contains(NeedKinds, spec.Kind) {
		return Request{}, fmt.Errorf("approve: --kind must be one of %s, got %q", strings.Join(NeedKinds, ", "), spec.Kind)
	}
	if strings.TrimSpace(spec.Title) == "" {
		return Request{}, errors.New("approve: a request needs --title")
	}
	if spec.Urgency == "" {
		spec.Urgency = "normal"
	}
	if !slices.Contains(NeedUrgencies, spec.Urgency) {
		return Request{}, fmt.Errorf("approve: --urgency must be one of %s, got %q", strings.Join(NeedUrgencies, ", "), spec.Urgency)
	}
	if spec.Recommend != "" && !hasOption(spec.Options, spec.Recommend) {
		return Request{}, fmt.Errorf("approve: --recommend %q is not one of the options", spec.Recommend)
	}
	spec.Risk = normTags(spec.Risk)
	for _, r := range spec.Risk {
		if r == "" {
			return Request{}, errors.New("approve: empty --risk tag")
		}
	}
	key := spec.Key
	if key == "" {
		key = spec.Kind + "\x00" + spec.Title
	}
	sprint := spec.Sprint
	if sprint == "" {
		sprint = strings.TrimSpace(os.Getenv("BASHY_SPRINT"))
	}
	if existing, ok := findPendingNeed(key); ok {
		return existing, nil
	}
	need := &Need{
		Kind: spec.Kind, Title: sanitizePrompt(spec.Title), Recommend: spec.Recommend,
		Refs: append([]string(nil), spec.Refs...), Deadline: spec.Deadline,
		Urgency: spec.Urgency, Risk: append([]string(nil), spec.Risk...),
		Key: key, Human: principalName(), From: callerAgent(), Route: RouteHuman,
	}
	if d, ok := ActiveDelegation(time.Now()); ok && d.covers(need, sprint) {
		need.Route, need.Delegate = RouteDelegate, d.To
	}
	r, err := Raise(ApprovalSpec{
		Source: SourceNeeds, Argv: []string{"needs", spec.Kind, need.Title},
		Sprint: sprint, Options: spec.Options, TTL: NeedTTL,
		Prompt: need.Title, Token: PlanToken([]string{key}), need: need,
	})
	return r, err
}

// normTags lower-cases and trims risk tags, so "Deploy" and " deploy" are the
// non-delegable "deploy".
func normTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, strings.ToLower(strings.TrimSpace(t)))
	}
	return out
}

func hasOption(opts []ApprovalOption, id string) bool {
	for _, o := range opts {
		if o.ID == id {
			return true
		}
	}
	return false
}

func findPendingNeed(key string) (Request, bool) {
	all, err := ListApprovals()
	if err != nil {
		return Request{}, false
	}
	for _, r := range all {
		if !r.IsNeed() || r.Approval.Need.Key != key {
			continue
		}
		if _, decided := LoadDecision(r.ID); !decided {
			return r, true
		}
	}
	return Request{}, false
}

// NeedState is the state a needs request is in: pending (with the delegate or
// the human), escalated (the delegate's deadline passed), or the verdict.
func NeedState(r Request, now time.Time) string {
	if d, ok := LoadDecision(r.ID); ok {
		if d.Via == ViaDelegate {
			return "answered-by-delegate"
		}
		return "answered-by-human"
	}
	n := r.Approval.Need
	if n.Route == RouteDelegate {
		if !n.Deadline.IsZero() && now.After(n.Deadline) {
			return "escalated"
		}
		if d, ok := ActiveDelegation(now); !ok || d.To != n.Delegate || !d.covers(n, r.Approval.Sprint) {
			// Delegation revoked, expired, given to another agent or narrowed
			// so this request is out of scope: it is the human's again.
			return "escalated"
		}
		return "delegated"
	}
	if !n.Deadline.IsZero() && now.After(n.Deadline) {
		return "overdue"
	}
	return "open"
}

// NeedsHuman reports whether an undecided needs request is waiting on the human
// (open, overdue or escalated), which is what the board alerts on.
func NeedsHuman(r Request, now time.Time) bool {
	if !r.IsNeed() {
		return false
	}
	switch NeedState(r, now) {
	case "open", "overdue", "escalated":
		return true
	}
	return false
}

// PendingNeeds returns undecided needs requests, oldest first.
func PendingNeeds() ([]Request, error) {
	all, err := PendingApprovals()
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, r := range all {
		if r.IsNeed() {
			out = append(out, r)
		}
	}
	return out, nil
}

// ErrNotYours refuses a decision by someone who is neither the request's human
// nor, inside its scope, their delegate.
var ErrNotYours = errors.New("approve: only the human this request is for, or their delegate inside its scope, may answer it")

// Caller says who decides. Human is set by a front end that authenticated a
// person (the board); otherwise the process environment decides.
type Caller struct {
	Human string
}

// checkNeedDecider authorises d on a needs request and fills its on-behalf
// fields. The rules:
//   - a human caller must be the request's human;
//   - an agent caller must be the active delegate, the request must still be
//     routed to it and inside the delegation's scope;
//   - everyone else is refused.
func checkNeedDecider(r Request, d *Decision, opt DecideOpts) error {
	n := r.Approval.Need
	if d.OptionID != "" && !hasOption(r.Approval.Options, d.OptionID) {
		return fmt.Errorf("approve: option %q is not one of this request's options", d.OptionID)
	}
	if opt.Caller.Human != "" {
		if !sameHuman(opt.Caller.Human, n.Human) {
			return ErrNotYours
		}
		d.By = opt.Caller.Human
		return nil
	}
	agent := callerAgent()
	if agent == "" {
		if !sameHuman(principalName(), n.Human) {
			return ErrNotYours
		}
		d.By = principalName()
		return nil
	}
	del, ok := ActiveDelegation(time.Now())
	if !ok || del.To != agent || n.Route != RouteDelegate || n.Delegate != agent ||
		!del.covers(n, r.Approval.Sprint) || NeedState(r, time.Now()) != "delegated" {
		return fmt.Errorf("%w (%s is not %s's delegate for this request)", ErrNotYours, agent, n.Human)
	}
	d.By, d.OnBehalfOf, d.Via = agent, n.Human, ViaDelegate
	return nil
}

func sameHuman(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.LastIndexAny(s, `\/`); i >= 0 {
			s = s[i+1:]
		}
		return s
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// ─── delegation ───────────────────────────────────────────────────────────────

// Delegation is the human's standing designation of an agent that answers
// needs requests on their behalf, inside a scope.
type Delegation struct {
	SchemaVersion string    `json:"schema_version"`
	Human         string    `json:"human"`
	To            string    `json:"to"`
	Kinds         []string  `json:"kinds,omitempty"`   // empty = every kind
	Sprints       []string  `json:"sprints,omitempty"` // empty = every sprint
	AllowRisk     []string  `json:"allow_risk,omitempty"`
	Created       time.Time `json:"created"`
	Expires       time.Time `json:"expires,omitzero"`
	Revoked       time.Time `json:"revoked,omitzero"`
}

const delegationFile = "delegation.json"

func delegationPath() string { return filepath.Join(Dir(), delegationFile) }

// covers reports whether a request falls inside the delegation's scope: its
// kind and sprint match and every risk tag on it is delegable (not in the
// default non-delegable set, unless the human allowed that tag).
func (d Delegation) covers(n *Need, sprint string) bool {
	if len(d.Kinds) > 0 && !slices.Contains(d.Kinds, n.Kind) {
		return false
	}
	if len(d.Sprints) > 0 && !slices.Contains(d.Sprints, sprint) {
		return false
	}
	for _, risk := range normTags(n.Risk) {
		if slices.Contains(NonDelegable, risk) && !slices.Contains(d.AllowRisk, risk) {
			return false
		}
	}
	return true
}

// LoadDelegation reads the delegation record, active or not.
func LoadDelegation() (Delegation, bool) {
	b, err := os.ReadFile(delegationPath())
	if err != nil {
		return Delegation{}, false
	}
	var d Delegation
	if json.Unmarshal(b, &d) != nil || d.To == "" {
		return Delegation{}, false
	}
	return d, true
}

// ActiveDelegation returns the delegation if it is neither revoked nor expired.
func ActiveDelegation(now time.Time) (Delegation, bool) {
	d, ok := LoadDelegation()
	if !ok || !d.Revoked.IsZero() || (!d.Expires.IsZero() && now.After(d.Expires)) {
		return Delegation{}, false
	}
	return d, true
}

// ErrHumanOnly refuses delegation changes from an agent: only the human
// designates, widens or revokes their delegate.
var ErrHumanOnly = errors.New("approve: only the human may designate or revoke a delegate")

// ErrOverrideHumanOnly refuses an override from an agent, the delegate
// included: only the human replaces a standing decision.
var ErrOverrideHumanOnly = errors.New("approve: only the human may override a decision")

// SetDelegation designates d.To as the caller's delegate, replacing any other.
func SetDelegation(d Delegation) (Delegation, error) {
	if callerAgent() != "" {
		return Delegation{}, ErrHumanOnly
	}
	if strings.TrimSpace(d.To) == "" {
		return Delegation{}, errors.New("approve: delegate needs --to NAME")
	}
	for _, k := range d.Kinds {
		if !slices.Contains(NeedKinds, k) {
			return Delegation{}, fmt.Errorf("approve: unknown kind %q (one of %s)", k, strings.Join(NeedKinds, ", "))
		}
	}
	d.AllowRisk = normTags(d.AllowRisk)
	d.SchemaVersion, d.Human, d.Created, d.Revoked = ApproveSchemaVersion, principalName(), time.Now(), time.Time{}
	if err := writeDelegation(d); err != nil {
		return Delegation{}, err
	}
	room.Record(room.EventApproval, d.Human, "delegate:"+d.To, "delegated", d.scopeLine())
	return d, nil
}

// RevokeDelegation ends the current delegation at once.
func RevokeDelegation() (Delegation, error) {
	if callerAgent() != "" {
		return Delegation{}, ErrHumanOnly
	}
	d, ok := LoadDelegation()
	if !ok || !d.Revoked.IsZero() {
		return Delegation{}, errors.New("approve: no active delegation to revoke")
	}
	d.Revoked = time.Now()
	if err := writeDelegation(d); err != nil {
		return Delegation{}, err
	}
	room.Record(room.EventApproval, d.Human, "delegate:"+d.To, "revoked", d.scopeLine())
	return d, nil
}

func (d Delegation) scopeLine() string {
	part := func(name string, v []string, all string) string {
		if len(v) == 0 {
			return name + "=" + all
		}
		return name + "=" + strings.Join(v, ",")
	}
	return strings.Join([]string{"to=" + d.To, part("kinds", d.Kinds, "all"),
		part("sprints", d.Sprints, "all"), part("allow-risk", d.AllowRisk, "none")}, " ")
}

func writeDelegation(d Delegation) error {
	if err := ensureDir(Dir()); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir(), ".delegation-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), delegationPath())
}

// ─── override ─────────────────────────────────────────────────────────────────

// Override replaces the standing decision on a needs request with the human's
// own. The prior decision is kept beside it (decision.N.json), so the record
// still says what the delegate decided and that the human overrode it.
//
// There is never a moment without a decision: the prior one is first linked to
// its archive name, then the new one is renamed over decision.json in one step.
// A plain answer racing the override therefore still meets ErrDecided, and a
// crash leaves either the old decision or the new one, never none. Overrides of
// one request are serialised by an exclusive lock file.
func Override(idOrPrefix string, d Decision, opt DecideOpts) (Request, error) {
	r, err := Find(idOrPrefix)
	if err != nil {
		return Request{}, err
	}
	if !r.IsNeed() {
		return Request{}, errors.New("approve: --override applies to needs requests only")
	}
	// Only the human overrides; an agent (the delegate included) is refused.
	if opt.Caller.Human == "" && callerAgent() != "" {
		return Request{}, ErrOverrideHumanOnly
	}
	if d.Verdict != VerdictAllow && d.Verdict != VerdictDeny {
		return Request{}, fmt.Errorf("ask: verdict must be allow or deny, got %q", d.Verdict)
	}
	if err := checkNeedDecider(r, &d, opt); err != nil {
		return Request{}, err
	}
	dir := requestDir(r.ID)
	unlock, err := lockOverride(dir)
	if err != nil {
		return Request{}, err
	}
	defer unlock()
	prior, ok := LoadDecision(r.ID)
	if !ok {
		return Request{}, errors.New("approve: nothing to override: the request is undecided")
	}
	n := 1
	for ; ; n++ {
		err := os.Link(filepath.Join(dir, decisionFile), filepath.Join(dir, fmt.Sprintf("decision.%d.json", n)))
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return Request{}, fmt.Errorf("approve: keeping the prior decision: %w", err)
		}
	}
	d.Always = false
	d.Via = firstNonEmpty(opt.Via, d.Via, ViaHuman)
	d.At = time.Now()
	d.Overrides = fmt.Sprintf("%s by %s via %s", prior.Verdict, prior.By, prior.Via)
	if err := replaceDecision(dir, d); err != nil {
		_ = os.Remove(filepath.Join(dir, fmt.Sprintf("decision.%d.json", n)))
		return Request{}, err
	}
	room.Record(room.EventApproval, d.By, approvalTarget(r), d.Verdict,
		fmt.Sprintf("%s via %s (token %s, overrides %s)", d.Verdict, d.Via, r.Approval.Token, d.Overrides))
	wakeRequester(r, d)
	return r, nil
}

// lockOverride takes the per-request override lock; a lock older than a minute
// is a crashed override's and is taken over.
func lockOverride(dir string) (func(), error) {
	path := filepath.Join(dir, "override.lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > time.Minute {
			_ = os.Remove(path)
			continue
		}
		break
	}
	return nil, errors.New("approve: another override of this request is in progress; try again")
}

// replaceDecision publishes d over decision.json in one rename.
func replaceDecision(dir string, d Decision) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".decision-*")
	if err != nil {
		return fmt.Errorf("ask: recording the decision: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, decisionFile))
}

// PriorDecisions lists the decisions an override replaced, oldest first.
func PriorDecisions(id string) []Decision {
	var out []Decision
	for n := 1; ; n++ {
		b, err := os.ReadFile(filepath.Join(requestDir(id), fmt.Sprintf("decision.%d.json", n)))
		if err != nil {
			return out
		}
		var d Decision
		if json.Unmarshal(b, &d) == nil {
			out = append(out, d)
		}
	}
}

// wakeRequester rings the agent that raised a needs request, so a waiting
// sprint manager continues: one subject-only notification to its inbox. Best
// effort: approve wait and the room timeline already carry the answer, and a
// requester that left no agent name has nobody to ring.
func wakeRequester(r Request, d Decision) {
	n := r.Approval.Need
	if n == nil || n.From == "" || n.From == d.By {
		return
	}
	subject := fmt.Sprintf("approve %s answered: %s", shortID(r.ID), d.Verdict)
	if d.OptionID != "" {
		subject += " " + d.OptionID
	}
	if d.OnBehalfOf != "" {
		subject += " by " + d.By + " for " + d.OnBehalfOf
	}
	_ = notifyFn(d.By, n.From, subject)
}

// notifyFn is bus.NotifyEvent, a seam for tests.
var notifyFn = bus.NotifyEvent

// findNeedByKey resolves key:K, the handle a requester can keep instead of an
// id: the undecided needs request raised with --key K, else the newest decided
// one.
func findNeedByKey(key string) (Request, error) {
	if strings.TrimSpace(key) == "" {
		return Request{}, errors.New("ask: key: needs a key")
	}
	if r, ok := findPendingNeed(key); ok {
		return r, nil
	}
	all, err := ListApprovals() // newest first
	if err != nil {
		return Request{}, err
	}
	for _, r := range all {
		if r.IsNeed() && r.Approval.Need.Key == key {
			return r, nil
		}
	}
	return Request{}, fmt.Errorf("ask: no needs request with key %q", key)
}
