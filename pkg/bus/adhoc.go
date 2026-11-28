package bus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

// AN AD HOC IDENTITY FOR A NATIVE AGENT SESSION.
//
// An agent started with its vendor CLI, not through `bashy <tool>`, calls bashy
// one command at a time. Nothing stamped it with a principal, so it had no
// name to sign with and no mailbox to be answered at. Each of its commands is
// a fresh child of the harness, so nothing one command exports reaches the
// next: the identity has to be found again every time, from something the
// session itself keeps constant.
//
// Two things are constant across a session's commands:
//
//   - the vendor's own session id, when the tool declares one in its
//     environment (CurrentSessionClaim reads it); and otherwise
//   - the harness PROCESS, which is always an ancestor of the command. It is
//     identified by pid and start time together, so a recycled pid is a
//     different session.
//
// The instance UUID is derived from that key, so every command of one session
// computes the same UUID with no registry to consult, and the label is the
// tool plus the UUID's first group — the same shape an interactive ad hoc
// session gets (chat.adHocID). The identity is then handed to the rest of the
// process through the environment the launcher would have set, so every
// existing path (authoring, the inbox, cursors) sees an ordinary instance.

// adHocNamespace scopes the derived UUIDs. It is a constant so two bashy
// processes derive the same UUID from the same session key.
var adHocNamespace = uuid.MustParse("6a1f0c1e-3c0b-5d53-9a4e-7b1d2f4c8e90")

// adHocGenerations bounds how many times a session whose derived instance was
// retired under it is given the next one.
const adHocGenerations = 4

// AdHocNotice receives the one line that tells a session the name it was just
// given. A host points it at stderr; nil keeps it silent.
var AdHocNotice io.Writer = os.Stderr

// AdHocIdentity turns the ad hoc identity on, and a host sets it only for the
// surfaces it is meant for: messaging (the board and the inbox). It is off by
// default because BoardIdentity and ResolveAuthoredActor also answer "who is
// this" for surfaces that grant authority — a meeting's decisions, a sprint's
// claims — and there an agent nobody named must stay refused.
var AdHocIdentity bool

// WithAdHocIdentity runs fn with the ad hoc identity on and then puts back
// both the switch and the environment an adoption stamps. It is for a caller
// that needs the session's mailbox for one question — "is there unread mail?"
// — inside a process that goes on to serve some other surface, which must not
// inherit an identity it would not have been given.
func WithAdHocIdentity(fn func()) {
	prior := AdHocIdentity
	type saved struct {
		value string
		set   bool
	}
	env := map[string]saved{}
	for _, k := range []string{principal.InstanceEnv, "BASHY_SESSION_CLAIM"} {
		v, ok := os.LookupEnv(k)
		env[k] = saved{v, ok}
	}
	AdHocIdentity = true
	defer func() {
		AdHocIdentity = prior
		for k, v := range env {
			if v.set {
				_ = os.Setenv(k, v.value)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}()
	fn()
}

// AdHocIdentityEnv set to "off" keeps the old behavior: an unattributed
// harness session is refused and told how to name itself.
const AdHocIdentityEnv = "BASHY_ADHOC_IDENTITY"

// harnessAncestor is a seam so a test can state which ancestor is the harness.
var harnessAncestor = ancestorNamed

var adHocLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-[0-9a-f]{8}$`)

// adoptAdHocIdentity gives an unattributed harness session an instance to act
// as, and reports whether it did. It does nothing when the caller already has
// an identity, named one, or is not an agent harness at all: a human, a
// script and a launcher-stamped agent are untouched.
func adoptAdHocIdentity(requested string) bool {
	if !AdHocIdentity || strings.TrimSpace(requested) != "" || DetectHarness == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(AdHocIdentityEnv)), "off") {
		return false
	}
	if strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")) != "" {
		return false
	}
	if _, ok := principal.SelfInstanceUUID(); ok {
		return false
	}
	if _, ok := declaredAgentIdentity(); ok {
		return false
	}
	tool, detected := DetectHarness()
	tool = strings.ToLower(strings.TrimSpace(tool))
	if !detected || tool == "" {
		return false
	}
	store := InstanceStoreFn()
	if store == nil {
		return false
	}
	key, synthetic, ok := adHocSessionKey(tool)
	if !ok {
		return false
	}
	inst, opened, err := adHocInstance(store, tool, key)
	if err != nil {
		return false
	}
	_ = os.Setenv(principal.InstanceEnv, inst.UUID)
	if synthetic {
		// No vendor session id: the ancestry key is the session claim, in the
		// variable a launcher uses to hand one down.
		_ = os.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim(key))
	}
	if opened && AdHocNotice != nil {
		fmt.Fprintf(AdHocNotice, "bashy: this %s session is %s — others reach it with `bashy mb send %s \"...\"`; read with `bashy inbox`\n",
			tool, inst.Label, inst.Label)
	}
	return true
}

// ErrAgentIDRequired reports an agent session that named nobody and that
// nothing could be derived for: no vendor session id and no harness process
// found above the command. It wraps ErrUnattributed.
var ErrAgentIDRequired = fmt.Errorf("%w: an agent id is required", ErrUnattributed)

// AgentIDGuidance is how such a session names itself. One place, so every
// refusal says the same thing.
func AgentIDGuidance() string {
	return "name this session, then run the command again:\n" +
		"  BASHY_AGENT=NAME bashy <command> ...     a name of your choosing, on every call\n" +
		"  bashy agent add NAME --tool T --model M  register it once, to be addressable by that name\n" +
		"  " + AdHocIdentityEnv + "=off                 run as before this check (the bare tool name)"
}

// StampAdHocIdentity is the front-door step: a native agent session that
// declared no identity is given its ad hoc instance and the process is stamped
// the way a bashy launcher stamps a session it starts, so every resolver in
// this process and every child sees one named agent and never the bare tool.
//
// stamped reports that this call did it. A caller that already has an
// identity — any of the launcher's variables, even an unregistered name it
// chose itself — is left exactly as it is, and so is a caller that is not an
// agent harness. An agent session with nothing to key an identity on returns
// ErrAgentIDRequired; a store that cannot hold one more instance returns
// neither, and the caller runs as it did before this step existed.
func StampAdHocIdentity() (inst fleet.Instance, stamped bool, err error) {
	if DetectHarness == nil || strings.EqualFold(strings.TrimSpace(os.Getenv(AdHocIdentityEnv)), "off") {
		return fleet.Instance{}, false, nil
	}
	for _, env := range []string{"BASHY_PRINCIPAL", principal.InstanceEnv, "BASHY_INSTANCE_UUID",
		"BASHY_AGENT", "WEAVE_AGENT", "BASHY_AGENT_ID", "WEAVE_CONDUCTOR"} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return fleet.Instance{}, false, nil
		}
	}
	tool, detected := DetectHarness()
	tool = strings.ToLower(strings.TrimSpace(tool))
	if !detected || tool == "" {
		return fleet.Instance{}, false, nil
	}
	store := InstanceStoreFn()
	if store == nil {
		return fleet.Instance{}, false, nil
	}
	key, synthetic, ok := adHocSessionKey(tool)
	if !ok {
		return fleet.Instance{}, false, fmt.Errorf("%w: running under %s with no declared identity, no session id and no %s process above this command",
			ErrAgentIDRequired, tool, tool)
	}
	inst, opened, oerr := adHocInstance(store, tool, key)
	if oerr != nil {
		return fleet.Instance{}, false, nil
	}
	for k, v := range map[string]string{
		principal.InstanceEnv:   inst.UUID,
		"BASHY_INSTANCE_UUID":   inst.UUID,
		"BASHY_INSTANCE_FAMILY": inst.FamilyID,
		"BASHY_INSTANCE_LABEL":  inst.Label,
		"BASHY_AGENT":           inst.Label,
	} {
		_ = os.Setenv(k, v)
	}
	if synthetic {
		_ = os.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim(key))
	}
	if opened && AdHocNotice != nil {
		fmt.Fprintf(AdHocNotice, "bashy: this %s session is %s — others reach it with `bashy mb send %s \"...\"`; read with `bashy inbox`\n",
			tool, inst.Label, inst.Label)
	}
	return inst, true, nil
}

// adHocSessionKey is what stays constant across one session's commands.
// synthetic reports a key made from the harness process rather than from the
// vendor's session id.
func adHocSessionKey(tool string) (key string, synthetic, ok bool) {
	if CurrentSessionClaim != nil {
		if raw := strings.TrimSpace(CurrentSessionClaim(tool)); raw != "" {
			return "session:" + raw, false, true
		}
	}
	pid, found := harnessAncestor(adHocHarnessNames(tool)...)
	if !found {
		return "", false, false
	}
	start, ok := room.PIDStart(pid)
	if !ok {
		return "", false, false
	}
	return "process:" + strconv.Itoa(pid) + ":" + start, true, true
}

// adHocHarnessNames are the executable names the harness may run under: the
// tool's name and its declared binary.
func adHocHarnessNames(tool string) []string {
	names := []string{tool}
	if t, ok := fleet.New().Tool(tool); ok {
		names = append(names, t.Binary())
	}
	return names
}

// adHocInstance returns the session's instance, opening it on first use.
func adHocInstance(store *fleet.InstanceStore, tool, key string) (fleet.Instance, bool, error) {
	fam := fleet.Family{Name: tool, Display: tool, Policy: fleet.PolicySingle, Bindings: []string{tool}}
	for gen := 0; gen < adHocGenerations; gen++ {
		name := tool + "\x00" + key
		if gen > 0 {
			name += "\x00" + strconv.Itoa(gen)
		}
		id := uuid.NewSHA1(adHocNamespace, []byte(name)).String()
		inst, err := store.Get(id)
		switch {
		case err == nil && inst.Active():
			return inst, false, nil
		case err == nil:
			continue // retired under the session: take the next generation
		case !errors.Is(err, fleet.ErrInstanceUnknown):
			return fleet.Instance{}, false, err
		}
		retireStaleAdHoc(store, fam)
		opts := fleet.OpenOptions{
			UUID:     id,
			Label:    room.AgentClaimID(tool + "-" + strings.ReplaceAll(id, "-", "")[:8]),
			MailFrom: BoardHeadSeq() + 1,
		}
		inst, err = store.Open(fam, opts)
		var full *fleet.CapError
		if errors.As(err, &full) && retireIdleAdHoc(store, full.Live) {
			inst, err = store.Open(fam, opts)
		}
		if err != nil {
			return fleet.Instance{}, false, err
		}
		return inst, true, nil
	}
	return fleet.Instance{}, false, fmt.Errorf("bus: no ad hoc instance left for this %s session", tool)
}

// adHocStaleAfter is how long an ad hoc instance nobody owns is kept. A native
// session has no exit hook to retire its instance, so the next session to open
// one sweeps those left behind; without that they would fill the family's cap.
const adHocStaleAfter = 24 * time.Hour

// retireStaleAdHoc retires the family's ad hoc instances that no live session
// owns and that were opened more than adHocStaleAfter ago. Best-effort.
func retireStaleAdHoc(store *fleet.InstanceStore, fam fleet.Family) {
	all, err := store.List()
	if err != nil {
		return
	}
	cutoff := adHocNow().Add(-adHocStaleAfter)
	for _, i := range all {
		if !i.Active() || i.FamilyID != fam.ID() || !adHocLabel.MatchString(i.Label) {
			continue
		}
		created, err := time.Parse(time.RFC3339, strings.TrimSpace(i.Created))
		if err != nil || created.After(cutoff) {
			continue
		}
		if _, owned := principal.InstanceOwner(i); owned {
			continue
		}
		_, _ = store.Retire(i.UUID, InstanceMail)
	}
}

// adHocNow is a seam so a test can age an instance.
var adHocNow = time.Now

// retireIdleAdHoc makes room in a full family by retiring its oldest ad hoc
// instance that no live session owns. Nothing else is ever retired here: a
// named or owned instance is somebody's, and a full family of those is the
// operator's to resolve.
func retireIdleAdHoc(store *fleet.InstanceStore, live []fleet.Instance) bool {
	idle := make([]fleet.Instance, 0, len(live))
	for _, i := range live {
		if !adHocLabel.MatchString(i.Label) {
			continue
		}
		if _, owned := principal.InstanceOwner(i); owned {
			continue
		}
		idle = append(idle, i)
	}
	if len(idle) == 0 {
		return false
	}
	sort.SliceStable(idle, func(a, b int) bool { return idle[a].Created < idle[b].Created })
	_, err := store.Retire(idle[0].UUID, InstanceMail)
	return err == nil
}
