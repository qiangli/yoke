package meet

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/todo"
)

// The exported adoption API — the CLI (adopt.go) and the browser routes
// (serve.go) both call these, so there is one truth and no second store: the
// stories are ordinary todo items in the register `bashy todo` and `bashy
// sprint` already use. Meet imports pkg/todo only; it never imports weave, which
// imports Meet.

// ErrNoAuthority marks an adoption attempted without explicit human authority.
var ErrNoAuthority = errors.New("meet: adoption needs an explicit human confirmation")

// ActionStore answers which story register the browser routes and API callers
// use when the request does not name one. An embedder may replace it.
var ActionStore = func() (*issue.Store, error) {
	st, _, err := todo.ResolveStore("", false, false, "")
	return st, err
}

type (
	AdoptResult      = adoptResult
	ReconcileFinding = reconcileFinding
)

// ActionView is one candidate action and its adoption state.
type ActionView struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Owner   string `json:"owner,omitempty"`
	Source  string `json:"source"` // "agreed" or "proposed"
	Adopted bool   `json:"adopted"`
	Story   string `json:"story,omitempty"`
}

// ActionSpec is the operator's explicit terms for one action.
type ActionSpec struct {
	Action  string   `json:"action"`
	Owner   string   `json:"owner,omitempty"`
	Depends []string `json:"depends,omitempty"`
	Accept  []string `json:"accept,omitempty"`
}

// AdoptRequest asks to adopt (Confirm) or preview adopting actions. By is the
// confirming human; a browser route fills it from the authenticated identity,
// never from the body.
type AdoptRequest struct {
	Actions []ActionSpec `json:"actions"`
	Confirm bool         `json:"confirm"`
	By      string       `json:"by,omitempty"`
	Sprint  int64        `json:"sprint,omitempty"`
	Store   *issue.Store `json:"-"`
}

// AdoptResponse is a preview or an adoption. Results is empty when no action was
// named and Candidates carries the listing.
type AdoptResponse struct {
	Room       string        `json:"room"`
	Preview    bool          `json:"preview"`
	Candidates []ActionView  `json:"candidates,omitempty"`
	Results    []AdoptResult `json:"results,omitempty"`
}

func storeFor(s *issue.Store) (*issue.Store, error) {
	if s != nil {
		return s, nil
	}
	return ActionStore()
}

// requireHumanAuthority refuses an adopter that is a seated agent: the secretary,
// the chair or any other participant. A synthesis or a proposal is never an
// authority; only a person naming the actions is.
func requireHumanAuthority(st *State, by string) error {
	who := strings.TrimSpace(strings.TrimPrefix(by, "@"))
	if who == "" {
		return fmt.Errorf("%w: no confirming person named", ErrNoAuthority)
	}
	canon := canonAgent(who)
	for _, seat := range append([]string{st.Secretary, st.chair()}, st.Participants...) {
		if seat != "" && (strings.EqualFold(seat, who) || strings.EqualFold(canonAgent(seat), canon)) {
			return fmt.Errorf("%w: %s is a seated agent, not the human who agrees the action", ErrNoAuthority, who)
		}
	}
	if _, ok := fleet.New().Agent(who); ok {
		return fmt.Errorf("%w: %s is an agent, not the human who agrees the action", ErrNoAuthority, who)
	}
	// An unknown name is not a human: only the room's own person, or the human
	// who convened it, holds the authority. Observers watch; they do not decide.
	if sameHuman(who, st.Human) || (st.initiatorKind() == "human" && sameHuman(who, st.initiatorName())) {
		return nil
	}
	return fmt.Errorf("%w: %s is neither this room's human nor the person who convened it", ErrNoAuthority, who)
}

func sameHuman(a, b string) bool {
	a, b = strings.TrimSpace(strings.TrimPrefix(a, "@")), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

// callerPrincipal is who is actually at the CLI: the OS user, provided no agent
// identity is attached to the process. A --by naming
// anyone else is refused — the flag cannot lend authority, it can only repeat
// who the caller already is.
func callerPrincipal(by string) (string, error) {
	// An agent session — launched (BASHY_AGENT, BASHY_PRINCIPAL) or a bare
	// harness (CLAUDECODE, …) — is never the room's human, whatever $USER says:
	// agents share the operator's OS account.
	if ref, ok := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self(); ok {
		return "", fmt.Errorf("%w: caller is an agent session (%s), not a human", ErrNoAuthority, ref.Name)
	}
	me := humanName()
	if by = strings.TrimSpace(by); by != "" && !strings.EqualFold(by, me) {
		return "", fmt.Errorf("%w: --by %s is not the calling user (%s)", ErrNoAuthority, by, me)
	}
	return me, nil
}

// roomMarked reports whether the room's transcript already carries a note with
// the idempotency marker, so a retry after a crash does not post it twice.
func roomMarked(room, marker string) bool {
	events, _ := readTranscript(room)
	for _, e := range events {
		if e.Kind == "note" && strings.Contains(e.Text, marker) {
			return true
		}
	}
	return false
}

// postOnce records a note unless the room already holds its marker.
func postOnce(st *State, by, marker, text string) error {
	if roomMarked(st.ID, marker) {
		return nil
	}
	_, err := record(st, "note", by, "", text+" ["+marker+"]")
	return err
}

// afterNotify is a test seam between the room post and the story mark.
var afterNotify func() error

// roomActions loads the room and its active candidate actions.
func roomActions(ref string) (*State, []agreedAction, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, nil, err
	}
	events, _ := readTranscript(st.ID)
	return st, agreedActions(st.ID, events, loadSynthesis(st.ID)), nil
}

// RoomActions lists the room's active candidate actions with their adoption
// state. It writes nothing.
func RoomActions(ref string, store *issue.Store) ([]ActionView, error) {
	st, current, err := roomActions(ref)
	if err != nil {
		return nil, err
	}
	reg, err := storeFor(store)
	if err != nil {
		return nil, err
	}
	adopted, err := adoptedStories(reg, st.ID)
	if err != nil {
		return nil, err
	}
	out := make([]ActionView, 0, len(current))
	for _, a := range current {
		v := ActionView{ID: a.ID, Text: a.Text, Owner: a.Owner, Source: a.Source}
		if it, ok := adopted[a.ID]; ok {
			v.Adopted, v.Story = true, it.ID
		}
		out = append(out, v)
	}
	return out, nil
}

// AdoptActions previews or adopts the named actions as linked sprint stories.
// Without Confirm nothing is written. With Confirm the caller must be a human
// (see requireHumanAuthority) and each action needs acceptance criteria. Adopting
// an action twice, concurrently or not, yields one story.
func AdoptActions(ref string, req AdoptRequest) (*AdoptResponse, error) {
	st, current, err := roomActions(ref)
	if err != nil {
		return nil, err
	}
	reg, err := storeFor(req.Store)
	if err != nil {
		return nil, err
	}
	resp := &AdoptResponse{Room: st.ID, Preview: !req.Confirm}
	if len(req.Actions) == 0 {
		views, err := RoomActions(ref, reg)
		resp.Candidates = views
		return resp, err
	}
	if req.Confirm {
		if err := requireHumanAuthority(st, req.By); err != nil {
			return nil, err
		}
	}
	byID := map[string]agreedAction{}
	for _, a := range current {
		byID[a.ID] = a
	}
	var selected []agreedAction
	specs := map[string]adoptSpec{}
	for _, sp := range req.Actions {
		a, ok := byID[sp.Action]
		if !ok {
			return nil, fmt.Errorf("meet: no active action %s in room %s (it may have been withdrawn)", sp.Action, st.ID)
		}
		if _, dup := specs[a.ID]; dup {
			continue
		}
		selected = append(selected, a)
		specs[a.ID] = adoptSpec{Owner: sp.Owner, Depends: sp.Depends, Accept: sp.Accept}
	}
	sprint := req.Sprint
	if sprint == 0 && todo.DefaultSprint != nil {
		if sprint, err = todo.DefaultSprint(); err != nil {
			return nil, err
		}
	}
	if sprint != 0 && todo.PrepareSprint != nil && req.Confirm {
		if err := todo.PrepareSprint(reg, sprint); err != nil {
			return nil, err
		}
	}
	resp.Results, err = adoptActions(st.ID, reg, selected, specs, strings.TrimSpace(req.By), sprint, req.Confirm)
	return resp, err
}

// ReconcileRequest asks which adopted work a changed decision affects, and with
// Confirm marks it and asks the room to reconcile.
type ReconcileRequest struct {
	Confirm bool         `json:"confirm"`
	By      string       `json:"by,omitempty"`
	Store   *issue.Store `json:"-"`
}

// ReconcileActions reports adopted stories whose action no longer stands in the
// room (retracted or re-decided) and the stories that depend on them. With
// Confirm each newly affected story is marked and the room is asked, once.
func ReconcileActions(ref string, req ReconcileRequest) ([]ReconcileFinding, error) {
	st, current, err := roomActions(ref)
	if err != nil {
		return nil, err
	}
	reg, err := storeFor(req.Store)
	if err != nil {
		return nil, err
	}
	if req.Confirm {
		if err := requireHumanAuthority(st, req.By); err != nil {
			return nil, err
		}
	}
	by := strings.TrimSpace(req.By)
	return reconcileAdopted(st.ID, reg, current, req.Confirm, func(f reconcileFinding) error {
		return postOnce(st, by, "reconcile-id:"+f.Story, fmt.Sprintf("reconcile: story %s (action %s) — %s. Please confirm, amend or close it.", f.Story, f.Action, f.Reason))
	})
}

// OutcomeRequest returns a verified story's result to its room.
type OutcomeRequest struct {
	Story    string       `json:"story"`
	Evidence []string     `json:"evidence"`
	By       string       `json:"by,omitempty"`
	Store    *issue.Store `json:"-"`
}

// ReturnOutcome posts a done (accepted) story's result and evidence links to the
// room it was adopted from, once. ref must name that room.
func ReturnOutcome(ref string, req OutcomeRequest) (story string, posted bool, err error) {
	st, err := roomOf(ref)
	if err != nil {
		return "", false, err
	}
	reg, err := storeFor(req.Store)
	if err != nil {
		return "", false, err
	}
	by := strings.TrimSpace(req.By)
	if by == "" {
		return "", false, errors.New("meet: an outcome needs a named reporter")
	}
	storyID := req.Story
	if it, err := todo.ResolveRef(reg, req.Story); err == nil {
		storyID = it.ID
		if r := extString(it, extRoom); r != "" && r != st.ID {
			return it.ID, false, fmt.Errorf("meet: story %s belongs to room %s, not %s", it.ID, r, st.ID)
		}
	}
	it, posted, err := recordOutcome(reg, req.Story, req.Evidence, func(room, text string) error {
		rs, err := loadState(room)
		if err != nil {
			return err
		}
		return postOnce(rs, by, "outcome-id:"+storyID, text)
	})
	if it != nil {
		story = it.ID
	}
	return story, posted, err
}
