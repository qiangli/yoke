// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

// An INSTANCE is one conversation.
//
// It owns a context, a mailbox and an ownership claim, and it is identified by
// a UUID — not by a name. That is the correction to the permanent-name design
// in docs/bashy-agent-identity-design.md: a name is something humans say and
// therefore something humans reuse, while a conversation's identity has to
// survive being renamed and must never be inherited by whatever is called that
// next. Keying on a UUID is what makes "retire Esme, then open Esme-2" safe —
// the label moves, the mailbox does not.
//
// Three facts follow from "the UUID is the identity", and every rule below is
// one of them restated:
//
//   - A FRESH context is a new UUID with an empty mailbox. It cannot read the
//     mail of whatever last held its label, because mail is addressed to the
//     UUID.
//   - A RESUME is the same UUID. Pending mail, the mailbox watermark and the
//     ownership claim all continue; a resumed agent that lost its queue would
//     silently drop work that was already accepted on its behalf.
//   - RETIRING archives mail and releases the LABEL. It does not delete the
//     record and it does not free the UUID: evidence outlives the worker, and
//     the cap counts live instances, not the ones that ever existed.
//
// # What lives here and what deliberately does not
//
// This file is pure data and disk — pkg/fleet is a leaf (leaf_test.go) and
// stays one. The OWNERSHIP CLAIM is enforced by pkg/room's single-live-session
// rule (room.ClaimSession) and the MAILBOX is the bus's existing address +
// cursor; an instance holds the small pieces of state those two need (the
// claim id, the mail address, the mailbox watermark) and neither of them is
// reimplemented here. No new store, no new transport.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"gopkg.in/yaml.v3"
)

// dirInstances is the instance store's directory name under the fleet root.
// Instances are not a catalog NOUN: they are host-local runtime records with
// no embedded baseline, no shared ring and nothing to seed, so they stay out
// of the assetring machinery rather than pretending to be assets.
const dirInstances = "instances"

// InstanceDirEnv redirects the instance store, for tests and sandboxed runs.
const InstanceDirEnv = "BASHY_INSTANCES_DIR"

// InstanceDir resolves the instance store root.
//
//	$BASHY_INSTANCES_DIR        the specific override
//	$BASHY_FLEET_DIR/instances  the whole fleet home relocated
//	~/.config/bashy/instances   the default
//
// It is HOST-GLOBAL on purpose. "At most one live owning session per instance"
// has to hold across repositories (#1245), and a per-checkout store would give
// every clone its own idea of who was live — which is exactly the collision
// that rule exists to refuse.
func InstanceDir() string {
	if d := strings.TrimSpace(os.Getenv(InstanceDirEnv)); d != "" {
		return d
	}
	return filepath.Join(DefaultRoot(), dirInstances)
}

// Instance is one conversation's identity record.
type Instance struct {
	// UUID is the identity. Assigned once, never reused, never renamed.
	UUID string `yaml:"uuid" json:"uuid"`

	// FamilyID, Policy and Bindings are the family configuration FROZEN at
	// open. They are copied rather than looked up so a later catalog edit
	// cannot retroactively change what a past conversation ran as — and so
	// Rebind has something immutable to compare against.
	FamilyID string   `yaml:"family_id" json:"family_id"`
	Family   string   `yaml:"family" json:"family"`
	Policy   string   `yaml:"policy,omitempty" json:"policy,omitempty"`
	Bindings []string `yaml:"bindings" json:"bindings"`

	// Label is the display name a human says ("Esme", "Esme-2"). It is held,
	// not owned: retiring releases it for reuse by a LATER instance, which is
	// why it must never be used to address mail.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`

	// Handle is an optional short address the instance chose for itself. It
	// must resolve unambiguously in its scope and it is NOT ownership proof —
	// anyone can type a handle; only a session claim owns an instance.
	Handle string `yaml:"handle,omitempty" json:"handle,omitempty"`

	Created string `yaml:"created" json:"created"`
	// Retired is the retirement timestamp. Non-empty means the label is free
	// and the instance no longer counts against the family's active cap. The
	// record itself stays.
	Retired string `yaml:"retired,omitempty" json:"retired,omitempty"`

	// MailFrom is the mailbox watermark: the bus sequence this instance's
	// personal mailbox BEGINS at. A fresh instance starts at the current
	// timeline head, which is what makes its mailbox empty without deleting
	// anything — a reused label inherits no backlog because the backlog is
	// below its watermark and addressed to another UUID besides.
	MailFrom int64 `yaml:"mail_from,omitempty" json:"mail_from,omitempty"`
	// MailArchive points at the retirement archive of this instance's mail.
	// Set by Retire. Mail is archived, never deleted.
	MailArchive string `yaml:"mail_archive,omitempty" json:"mail_archive,omitempty"`
}

// Active reports an instance that has not been retired.
func (i Instance) Active() bool { return strings.TrimSpace(i.Retired) == "" }

// MailAddress is the instance's PERSONAL mail address.
//
// It is derived from the UUID, not the label, and that is the whole point:
// personal mail stays with the instance that was addressed. Role mail
// (conductor:321 and friends) is NOT this — it keeps the bus's existing role
// topics, so it belongs to the responsibility and survives a holder handoff.
func (i Instance) MailAddress() string {
	if strings.TrimSpace(i.UUID) == "" {
		return ""
	}
	return "instance/" + i.UUID
}

// ClaimID is the room membership id that carries this instance's ownership.
// Namespaced so it can never be confused with an agent's or a person's
// singleton card.
func (i Instance) ClaimID() string {
	if strings.TrimSpace(i.UUID) == "" {
		return ""
	}
	return "instance:" + i.UUID
}

// URN is the principal form an external session exports once
// (BASHY_PRINCIPAL=dhnt:agent/<uuid>) so that neither its inbox reads nor its
// authored commands need a repeated --as.
//
// The kind is AGENT, not a new "instance" kind: an instance is an agent
// principal whose name is a UUID rather than a nickname, so every resolver
// that already understands agents understands this without being changed. See
// principal.InstanceURN, which is where that decision is argued.
func (i Instance) URN() string {
	canonical, err := ParseInstanceUUID(i.UUID)
	if err != nil {
		return ""
	}
	return "dhnt:agent/" + canonical
}

// Rebind always refuses. It exists so the refusal has ONE place and one
// message: changing the binding set or the selection policy is a new family
// version, and a new family version needs a new instance plus a handoff.
//
// Selecting among the models already inside a predefined composite is a
// different operation and is allowed — see Instance.Select.
func (i Instance) Rebind(Family) error { return ErrBindingImmutable }

// Select picks the binding for the next turn out of the instance's FROZEN set.
// Inside the set the instance keeps its UUID, mail and ownership; outside it,
// ErrBindingImmutable.
func (i Instance) Select(binding string) (string, error) {
	return Family{Policy: i.Policy, Bindings: i.Bindings}.Select(binding)
}

// InstanceStore is the on-disk set of instance records: one YAML file per
// UUID, in one flat directory. There is no index — the directory IS the index,
// for the same reason the room has no sweeper: a derived index is a second
// truth that can disagree with the first.
type InstanceStore struct {
	dir string
	// cap is the per-family limit on CONCURRENTLY ACTIVE instances. Zero means
	// DefaultInstanceCap; negative means unlimited.
	cap int
}

// DefaultInstanceCap bounds concurrent live conversations per family. It is a
// capacity guard, not a quality judgment: the limit exists because every live
// instance is a real subscription seat and a real context being paid for.
const DefaultInstanceCap = 8

// InstanceCapEnv overrides the per-family active cap.
const InstanceCapEnv = "BASHY_INSTANCE_CAP"

// NewInstanceStore opens the host's instance store. An empty dir means
// InstanceDir().
func NewInstanceStore(dir string) *InstanceStore {
	if strings.TrimSpace(dir) == "" {
		dir = InstanceDir()
	}
	return &InstanceStore{dir: dir, cap: envInstanceCap()}
}

// WithCap returns a store with an explicit per-family active cap. Negative is
// unlimited.
func (s *InstanceStore) WithCap(n int) *InstanceStore {
	return &InstanceStore{dir: s.dir, cap: n}
}

// Cap reports the effective per-family active cap.
func (s *InstanceStore) Cap() int {
	if s.cap == 0 {
		return DefaultInstanceCap
	}
	return s.cap
}

func envInstanceCap() int {
	v := strings.TrimSpace(os.Getenv(InstanceCapEnv))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// Dir reports the store's directory.
func (s *InstanceStore) Dir() string { return s.dir }

// The two ways an open can fail that a caller must tell apart.
var (
	// ErrInstanceUnknown names no record. A resume of a UUID that was never
	// opened is this, and it must not silently mint one: an instance whose
	// identity appeared from nowhere has no provenance for its mail.
	ErrInstanceUnknown = errors.New("fleet: names no instance")
	// ErrLabelHeld reports a display label already held by a LIVE instance.
	// Retired labels are free, which is the Esme-2 case.
	ErrLabelHeld = errors.New("fleet: display label is held by a live instance")
	// ErrHandleAmbiguous reports a handle that resolves to more than one live
	// instance. Refusing with choices beats guessing — the same rule whois and
	// ResolvePrincipal already follow.
	ErrHandleAmbiguous = errors.New("fleet: handle names more than one live instance")
)

// CapError reports an exhausted per-family active cap.
//
// It carries the family, the limit and the live instances BECAUSE the operator
// has to do something next, and "cap reached" alone does not say what. Every
// field here appears in the message: which family, how many are allowed, which
// UUIDs and labels are holding the slots, and the two ways out.
type CapError struct {
	FamilyID string
	Family   string
	Cap      int
	Live     []Instance
}

func (e *CapError) Error() string {
	var held []string
	for _, i := range e.Live {
		label := i.Label
		if label == "" {
			label = "(unlabelled)"
		}
		held = append(held, label+" "+i.UUID)
	}
	name := e.Family
	if name == "" {
		name = e.FamilyID
	}
	return fmt.Sprintf(
		"fleet: family %s already has %d/%d active instances (%s); retire one with `bashy instance retire <uuid>` or raise the cap with %s",
		name, len(e.Live), e.Cap, strings.Join(held, ", "), InstanceCapEnv)
}

// OpenOptions are the choices a caller makes when starting a FRESH context.
type OpenOptions struct {
	// Label is the display label to hold. Empty means "derive the next free one
	// from the family's display name" (Esme, Esme-2, …).
	Label string
	// Handle is an optional self-chosen short address.
	Handle string
	// MailFrom is the bus sequence the new mailbox begins at — normally the
	// caller's current timeline head. It is passed IN rather than read here so
	// pkg/fleet stays a leaf and so a test can state the watermark exactly.
	MailFrom int64
}

// Open starts a FRESH context on a family: a new UUID and an empty mailbox.
//
// The family's configuration is frozen into the record at this moment. Nothing
// about the new instance is inherited from whatever last held its label.
// The cap count, the label choice and the handle check are all made against
// the roster and then written, so the whole sequence is under the store lock —
// see withLock for what each of the three races produced without it.
func (s *InstanceStore) Open(f Family, opts OpenOptions) (Instance, error) {
	if err := f.validate(); err != nil {
		return Instance{}, err
	}
	var inst Instance
	err := s.withLock("open instance", func() error {
		all, err := s.List()
		if err != nil {
			return err
		}
		famID := f.ID()

		var live []Instance
		for _, i := range all {
			if i.Active() && i.FamilyID == famID {
				live = append(live, i)
			}
		}
		if limit := s.Cap(); limit >= 0 && len(live) >= limit {
			return &CapError{FamilyID: famID, Family: f.Name, Cap: limit, Live: live}
		}

		label := strings.TrimSpace(opts.Label)
		if label == "" {
			label = nextLabel(labelBase(f), all)
		} else if err := checkAddressFree(label, "", all); err != nil {
			return err
		}
		if h := strings.TrimSpace(opts.Handle); h != "" {
			if err := checkAddressFree(h, "", all); err != nil {
				return err
			}
			// A label and a handle on ONE instance may not collide with each
			// other either: they live in one address scope, so an instance
			// labelled Esme with handle esme would make `esme` ambiguous
			// against itself.
			if strings.EqualFold(h, label) {
				return fmt.Errorf("%w: %s is both the label and the handle", ErrHandleAmbiguous, h)
			}
		}

		inst = Instance{
			UUID:     uuid.NewString(),
			FamilyID: famID,
			Family:   f.Name,
			Policy:   f.Policy,
			Bindings: f.normalizedBindings(),
			Label:    label,
			Handle:   strings.TrimSpace(opts.Handle),
			Created:  nowStamp(),
			MailFrom: opts.MailFrom,
		}
		return s.write(inst)
	})
	if err != nil {
		return Instance{}, err
	}
	return inst, nil
}

// Resume returns an EXISTING instance unchanged.
//
// It is a read, and that is the contract: the UUID, the mailbox watermark, the
// pending mail below it and the ownership record all stay exactly as they
// were. Resuming a retired instance is refused — its mail has been archived
// and its label possibly re-issued, so continuing it would reopen a
// conversation whose name now means somebody else.
func (s *InstanceStore) Resume(id string) (Instance, error) {
	inst, err := s.Get(id)
	if err != nil {
		return Instance{}, err
	}
	if !inst.Active() {
		return Instance{}, fmt.Errorf("fleet: instance %s was retired at %s; open a new instance instead", inst.UUID, inst.Retired)
	}
	return inst, nil
}

// Get reads one record by UUID, retired or not.
//
// The id is canonicalized before it is used as a path, and the record's own
// UUID is VALIDATED against it after the read. Both halves of finding 4: the
// first stops an id that is not a UUID from naming a file, and the second
// stops a file whose contents disagree with its name from answering for an
// identity it does not hold — which is what would happen to a hand-edited or
// hand-copied record, and the mailbox is keyed on that UUID.
func (s *InstanceStore) Get(id string) (Instance, error) {
	canonical, err := ParseInstanceUUID(id)
	if err != nil {
		return Instance{}, err
	}
	b, err := os.ReadFile(s.path(canonical))
	if err != nil {
		if os.IsNotExist(err) {
			// "Not found" is only an unknown id when the store itself is sound:
			// Windows reports a lookup beneath a regular file as not-found too.
			if derr := checkMissingDir(s.dir); derr != nil {
				return Instance{}, derr
			}
			return Instance{}, fmt.Errorf("%w: %s", ErrInstanceUnknown, canonical)
		}
		return Instance{}, err
	}
	var inst Instance
	if err := yaml.Unmarshal(b, &inst); err != nil {
		return Instance{}, fmt.Errorf("fleet: instance %s: %w", canonical, err)
	}
	recorded, err := ParseInstanceUUID(inst.UUID)
	if err != nil {
		return Instance{}, fmt.Errorf("fleet: instance record %s: %w", canonical, err)
	}
	if recorded != canonical {
		return Instance{}, fmt.Errorf("fleet: instance record %s declares uuid %s; refusing to answer for an identity it does not hold", canonical, recorded)
	}
	inst.UUID = recorded
	return inst, nil
}

// Retire archives an instance's mail, releases its display label, and leaves
// the record in place.
//
// mail supplies the instance's PERSONAL mail, and it is required. The earlier
// version of this function created an empty archive file and called that
// "archived, not deleted" — a pointer to a file with nothing in it, which is
// indistinguishable from having dropped the mail and is worse, because it
// looks like evidence. Finding 5. pkg/fleet cannot read the bus itself (it is
// a leaf), so the records arrive through the seam; passing nil is refused
// rather than treated as "no mail", since every caller that has not been wired
// up yet would otherwise archive nothing and report success.
//
// The caller's records are retained UNDER THE OLD UUID and the archive is
// keyed on it, so mail addressed to a retired instance still resolves to that
// instance's evidence — the UUID keeps its delivery meaning after retirement.
// What retirement releases is the LABEL, and only the label.
//
// archive is where the mail went. It is returned rather than merely logged
// because "archived, not deleted" is a claim somebody will have to check (see
// ArchivedMail, which reads it back).
func (s *InstanceStore) Retire(id string, mail MailSource) (archive string, err error) {
	if mail == nil {
		return "", ErrMailSourceRequired
	}
	// The mail is collected BEFORE the store lock: the source reads the bus,
	// which has locks of its own, and holding ours across that call is a
	// lock-order hazard. The state is re-read under the lock below.
	inst, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if !inst.Active() {
		// Already retired: idempotent, and it must NOT archive again.
		return inst.MailArchive, nil
	}
	records, err := mail(inst)
	if err != nil {
		return "", fmt.Errorf("fleet: collect mail for instance %s: %w", inst.UUID, err)
	}
	err = s.withLock("retire instance", func() error {
		inst, err := s.Get(id)
		if err != nil {
			return err
		}
		if !inst.Active() {
			archive = inst.MailArchive
			return nil
		}
		// The mail is archived BEFORE the record is marked retired. If the
		// append fails the instance stays live and its label stays held, which
		// is the recoverable order: a retired instance whose mail never
		// reached the archive has released its label on a promise it did not
		// keep. A retry after a failure between the two steps skips the
		// records the archive already holds.
		archive, err = s.writeArchive(inst, records, true)
		if err != nil {
			return err
		}
		inst.Retired = nowStamp()
		inst.MailArchive = archive
		// The label is RELEASED, not blanked: `instance show` on a retired
		// UUID still has to say which label it held, or past evidence stops
		// resolving. Freedom to reuse the label comes from Retired being set —
		// checkAddressFree only considers live instances. The pointer and the
		// retired marker land in this ONE record write.
		return s.write(inst)
	})
	if err != nil {
		return "", err
	}
	return archive, nil
}

// SetHandle records an instance's optional self-chosen handle, refusing one
// that would be ambiguous among live instances.
func (s *InstanceStore) SetHandle(id, handle string) (Instance, error) {
	var inst Instance
	err := s.withLock("set instance handle", func() error {
		var err error
		if inst, err = s.Resume(id); err != nil {
			return err
		}
		h := strings.TrimSpace(handle)
		all, err := s.List()
		if err != nil {
			return err
		}
		if err := checkAddressFree(h, inst.UUID, all); err != nil {
			return err
		}
		// An instance may not take a handle equal to its OWN label either:
		// the two would be two spellings of one address and ResolveHandle
		// would have to pick between them.
		if h != "" && strings.EqualFold(h, strings.TrimSpace(inst.Label)) {
			return fmt.Errorf("%w: %s is already this instance's label", ErrHandleAmbiguous, h)
		}
		inst.Handle = h
		return s.write(inst)
	})
	if err != nil {
		return Instance{}, err
	}
	return inst, nil
}

// List returns every record, retired included, oldest first.
func (s *InstanceStore) List() ([]Instance, error) {
	// Checked up front, not only on a ReadDir error: on Windows, reading a
	// regular file as a directory is not a reliable error, and a store that
	// silently lists as empty turns a broken path into "no instances".
	if err := checkMissingDir(s.dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var inst Instance
		// A garbled record is skipped rather than fatal: one bad file must not
		// make the whole roster unreadable, which would wedge every open.
		if yaml.Unmarshal(b, &inst) != nil {
			continue
		}
		// A record whose uuid is not a UUID is garbled in the way that matters
		// most — the field is the identity — so it is skipped on the same
		// grounds, and never canonicalized into something plausible.
		canonical, err := ParseInstanceUUID(inst.UUID)
		if err != nil {
			continue
		}
		// A record whose filename disagrees with its uuid is skipped rather
		// than counted. It would otherwise occupy a cap slot and hold a label
		// under one identity while Get refuses to return it under either.
		if e.Name() != canonical+ext {
			continue
		}
		inst.UUID = canonical
		out = append(out, inst)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].UUID < out[j].UUID
	})
	return out, nil
}

// ActiveInstances returns the live instances of one family, or of every family
// when familyID is empty. This is what the cap counts.
func (s *InstanceStore) ActiveInstances(familyID string) ([]Instance, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, i := range all {
		if !i.Active() {
			continue
		}
		if familyID != "" && i.FamilyID != familyID {
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

// ResolveLabel maps a display label to the LIVE instance holding it. A retired
// instance never answers to its old label — that is what makes reusing Esme-2
// safe.
func (s *InstanceStore) ResolveLabel(label string) (Instance, bool, error) {
	all, err := s.List()
	if err != nil {
		return Instance{}, false, err
	}
	want := strings.ToLower(strings.TrimSpace(label))
	if want == "" {
		return Instance{}, false, nil
	}
	for _, i := range all {
		if i.Active() && strings.ToLower(strings.TrimSpace(i.Label)) == want {
			return i, true, nil
		}
	}
	return Instance{}, false, nil
}

// ResolveHandle maps a handle (or a label) to one live instance. Ambiguity is
// an error; a handle is an address, never ownership proof.
func (s *InstanceStore) ResolveHandle(handle string) (Instance, bool, error) {
	all, err := s.List()
	if err != nil {
		return Instance{}, false, err
	}
	return resolveHandle(handle, all)
}

// NextLabel reports the next free display label for a base name: the base
// itself when nothing live holds it, then base-2, base-3, …
//
// Esme-2 is a NEW label on a NEW identity, not a revival of Esme. Reuse the
// label; never the retired instance.
func (s *InstanceStore) NextLabel(base string) (string, error) {
	all, err := s.List()
	if err != nil {
		return "", err
	}
	return nextLabel(base, all), nil
}

func resolveHandle(handle string, all []Instance) (Instance, bool, error) {
	want := strings.ToLower(strings.TrimSpace(handle))
	if want == "" {
		return Instance{}, false, nil
	}
	var hits []Instance
	for _, i := range all {
		if !i.Active() {
			continue
		}
		if strings.ToLower(strings.TrimSpace(i.Handle)) == want ||
			strings.ToLower(strings.TrimSpace(i.Label)) == want {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return Instance{}, false, nil
	case 1:
		return hits[0], true, nil
	default:
		return Instance{}, false, fmt.Errorf("%w: %s", ErrHandleAmbiguous, strings.TrimSpace(handle))
	}
}

// checkAddressFree refuses a label or handle that any LIVE instance already
// answers to — in EITHER direction.
//
// Labels and handles share one address scope, because ResolveHandle resolves
// both and a human typing a name does not say which kind it is. So the checks
// have to cross: a new LABEL must not collide with an existing HANDLE (which
// Open previously did not check, so `Esme` could be opened while another
// instance already answered to the handle `esme`), and a new HANDLE must not
// collide with an existing LABEL. Checking only like against like made the
// scope ambiguous through the gap between the two sets.
//
// except is a UUID to ignore — the instance being updated, which is allowed to
// keep its own addresses.
func checkAddressFree(name, except string, all []Instance) error {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return nil
	}
	for _, i := range all {
		if !i.Active() || strings.EqualFold(i.UUID, except) {
			continue
		}
		if strings.ToLower(strings.TrimSpace(i.Label)) == want {
			return fmt.Errorf("%w: %s (held by %s)", ErrLabelHeld, strings.TrimSpace(name), i.UUID)
		}
		if strings.ToLower(strings.TrimSpace(i.Handle)) == want {
			return fmt.Errorf("%w: %s (handle of %s)", ErrHandleAmbiguous, strings.TrimSpace(name), i.UUID)
		}
	}
	return nil
}

// addressTaken reports a label or handle a live instance already answers to.
func addressTaken(name string, all []Instance) bool {
	return checkAddressFree(name, "", all) != nil
}

func nextLabel(base string, all []Instance) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "instance"
	}
	// A DERIVED label skips handles too, for the same reason an explicit one
	// is refused against them: Esme-2 is worth nothing if `Esme-2` already
	// resolves to somebody else's chosen handle.
	if !addressTaken(base, all) {
		return base
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !addressTaken(candidate, all) {
			return candidate
		}
	}
}

// labelBase is the display name an instance's label is drawn from: the
// family's own display label when it has one, else its catalog name.
func labelBase(f Family) string {
	if d := strings.TrimSpace(f.Display); d != "" {
		return d
	}
	return strings.TrimSpace(f.Name)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ParseInstanceUUID canonicalizes an instance id: parsed as a UUID and
// rendered in the one lower-case hyphenated form.
//
// Every lookup goes through it, and that is finding 4. The previous version
// mapped an id to a filename by replacing unsafe characters with "-", which is
// a LOSSY alias: "../x" and ".-/x" named one file, an upper-case UUID named a
// different file from the same UUID written in lower case, and the garbage id
// "!!!" resolved to a file called "-". A store addressed by an alias is a
// store where two ids can silently share a mailbox and where an id from a flag
// can point outside the directory. Parsing refuses all of it up front: either
// the caller named a UUID or it named nothing.
func ParseInstanceUUID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", fmt.Errorf("%w: (empty)", ErrInstanceUnknown)
	}
	u, err := uuid.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a UUID", ErrInstanceMalformed, trimmed)
	}
	return u.String(), nil
}

// ErrInstanceMalformed reports an id that is not a UUID at all.
//
// It is separate from ErrInstanceUnknown because the two mean different things
// to the caller: "no such instance" is a question about the store, while "that
// is not a UUID" is a question about the argument, and only the second is
// still wrong after the store changes.
var ErrInstanceMalformed = errors.New("fleet: instance id is not a UUID")

// lockPath is the ONE interprocess lock guarding this store.
//
// It sits inside the store dir and ends in .lock, so List (which reads only
// *.yaml) does not see it as a record. One lock for the whole store rather
// than one per record, because every check this store makes is a check ACROSS
// records — the per-family cap, the label set, the handle set — so a per-record
// lock would serialize exactly the writes that do not conflict and leave the
// cross-record reads unprotected.
func (s *InstanceStore) lockPath() string { return filepath.Join(s.dir, "instances.lock") }

// withLock runs fn with the store's interprocess lock held.
//
// Finding 1: Open, SetHandle and Retire each read the roster, decide against
// it, and write — and without one lock around all three steps the decision is
// made against a roster that another process is changing. Concretely: two
// concurrent opens on a family at cap-1 both counted 0 live instances and both
// wrote, so the cap silently admitted two; two opens picking a derived label
// both found "Esme" free; and two SetHandle calls both found a handle
// unambiguous and made it ambiguous. The lock is interprocess because the
// store is host-global by design (see InstanceDir) and the colliding writers
// are separate `bashy` processes in separate checkouts, not goroutines.
func (s *InstanceStore) withLock(intent string, fn func() error) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	lock, err := lockfile.Acquire(s.lockPath(), lockfile.Holder{
		Name: "instances", PID: os.Getpid(), Intent: intent,
	})
	if err != nil {
		return fmt.Errorf("fleet: serialize instance store: %w", err)
	}
	defer lock.Release()
	return fn()
}

// checkMissingDir returns nil when dir is a directory or is genuinely absent
// (it, or some ancestor, does not exist beneath an existing directory), and an
// error when the path is broken: dir — or the nearest existing ancestor — is a
// regular file, or a stat fails for any reason other than not-exist
// (permission, I/O). Callers use it before reading "not found" as "empty".
//
// The ancestor walk is the load-bearing half on Windows: there a lookup
// beneath a regular file fails with ERROR_PATH_NOT_FOUND, which os.IsNotExist
// accepts, where unix reports ENOTDIR. Without it a store path whose parent is
// a file is indistinguishable from a fresh host.
func checkMissingDir(dir string) error {
	p := filepath.Clean(dir)
	for {
		fi, err := os.Stat(p)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("fleet: store path %s: %s is not a directory", dir, p)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

func (s *InstanceStore) path(id string) string {
	// id is already canonical here: every caller parses it first. A UUID in
	// canonical form contains only hex and hyphens, so it cannot escape the
	// store directory and needs no sanitizing — which is the point of parsing
	// instead of sanitizing.
	return filepath.Join(s.dir, id+ext)
}

func (s *InstanceStore) write(i Instance) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(i)
	if err != nil {
		return err
	}
	path := s.path(i.UUID)
	tmp, err := os.CreateTemp(s.dir, ".instance-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	// Rename, so a reader never sees a half-written record.
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// There is deliberately no filename-sanitizing helper here any more. See
// ParseInstanceUUID: the id is PARSED, so there is nothing left to sanitize,
// and a lossy mapping from id to filename was the bug.
