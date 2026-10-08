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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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
// (BASHY_PRINCIPAL=dhnt:instance/<uuid>) so that neither its inbox reads nor
// its authored commands need a repeated --as.
func (i Instance) URN() string {
	if strings.TrimSpace(i.UUID) == "" {
		return ""
	}
	return "dhnt:instance/" + i.UUID
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
func (s *InstanceStore) Open(f Family, opts OpenOptions) (Instance, error) {
	if len(f.Bindings) == 0 {
		return Instance{}, ErrFamilyEmpty
	}
	all, err := s.List()
	if err != nil {
		return Instance{}, err
	}
	famID := f.ID()

	var live []Instance
	for _, i := range all {
		if i.Active() && i.FamilyID == famID {
			live = append(live, i)
		}
	}
	if limit := s.Cap(); limit >= 0 && len(live) >= limit {
		return Instance{}, &CapError{FamilyID: famID, Family: f.Name, Cap: limit, Live: live}
	}

	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = nextLabel(labelBase(f), all)
	} else if labelHeld(label, all) {
		return Instance{}, fmt.Errorf("%w: %s", ErrLabelHeld, label)
	}
	if h := strings.TrimSpace(opts.Handle); h != "" {
		if _, found, err := resolveHandle(h, all); err != nil {
			return Instance{}, err
		} else if found {
			return Instance{}, fmt.Errorf("%w: %s", ErrHandleAmbiguous, h)
		}
	}

	inst := Instance{
		UUID:     uuid.NewString(),
		FamilyID: famID,
		Family:   f.Name,
		Policy:   f.Policy,
		Bindings: append([]string{}, f.Bindings...),
		Label:    label,
		Handle:   strings.TrimSpace(opts.Handle),
		Created:  nowStamp(),
		MailFrom: opts.MailFrom,
	}
	if err := s.write(inst); err != nil {
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
func (s *InstanceStore) Get(id string) (Instance, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Instance{}, ErrInstanceUnknown
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return Instance{}, fmt.Errorf("%w: %s", ErrInstanceUnknown, id)
		}
		return Instance{}, err
	}
	var inst Instance
	if err := yaml.Unmarshal(b, &inst); err != nil {
		return Instance{}, fmt.Errorf("fleet: instance %s: %w", id, err)
	}
	if inst.UUID == "" {
		return Instance{}, fmt.Errorf("%w: %s", ErrInstanceUnknown, id)
	}
	return inst, nil
}

// Retire archives an instance's mail, releases its display label, and leaves
// the record in place.
//
// archive is where the mail went. It is returned rather than merely logged
// because "archived, not deleted" is a claim somebody will have to check.
func (s *InstanceStore) Retire(id string) (archive string, err error) {
	inst, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if !inst.Active() {
		return inst.MailArchive, nil
	}
	archive = filepath.Join(s.dir, "archive", inst.UUID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		return "", err
	}
	// Touch the archive so the pointer is never a promise about a path that
	// does not exist. The bus writes the retained records into it (see
	// ArchiveInstanceMail); an instance that received nothing archives empty,
	// which is a true statement and not a missing file.
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	inst.Retired = nowStamp()
	inst.MailArchive = archive
	// The label is RELEASED, not blanked: `instance show` on a retired UUID
	// still has to say which label it held, or past evidence stops resolving.
	// Freedom to reuse the label comes from Retired being set — labelHeld only
	// considers live instances.
	if err := s.write(inst); err != nil {
		return "", err
	}
	return archive, nil
}

// SetHandle records an instance's optional self-chosen handle, refusing one
// that would be ambiguous among live instances.
func (s *InstanceStore) SetHandle(id, handle string) (Instance, error) {
	inst, err := s.Resume(id)
	if err != nil {
		return Instance{}, err
	}
	h := strings.TrimSpace(handle)
	all, err := s.List()
	if err != nil {
		return Instance{}, err
	}
	if h != "" {
		for _, other := range all {
			if other.UUID == inst.UUID || !other.Active() {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(other.Handle), h) || strings.EqualFold(strings.TrimSpace(other.Label), h) {
				return Instance{}, fmt.Errorf("%w: %s", ErrHandleAmbiguous, h)
			}
		}
	}
	inst.Handle = h
	if err := s.write(inst); err != nil {
		return Instance{}, err
	}
	return inst, nil
}

// List returns every record, retired included, oldest first.
func (s *InstanceStore) List() ([]Instance, error) {
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
		if yaml.Unmarshal(b, &inst) != nil || inst.UUID == "" {
			continue
		}
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

func labelHeld(label string, all []Instance) bool {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, i := range all {
		if i.Active() && strings.ToLower(strings.TrimSpace(i.Label)) == want {
			return true
		}
	}
	return false
}

func nextLabel(base string, all []Instance) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "instance"
	}
	if !labelHeld(base, all) {
		return base
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !labelHeld(candidate, all) {
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

func (s *InstanceStore) path(id string) string {
	// A UUID is already path-safe, but the id arrives from a flag and an
	// environment variable, so it is sanitized rather than trusted: nothing
	// here may address a file outside the store.
	safe := instanceFileName(id)
	return filepath.Join(s.dir, safe+ext)
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

var instanceUnsafeFile = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// instanceFileName reduces an id to the characters a filename may hold
// on every host we run on. It is deliberately lossy and deliberately NOT a
// hash: a store you can read with ls is worth more than one whose filenames
// round-trip.
func instanceFileName(id string) string {
	cleaned := instanceUnsafeFile.ReplaceAllString(strings.TrimSpace(id), "-")
	cleaned = strings.Trim(cleaned, "-.")
	if cleaned == "" {
		return "instance"
	}
	return cleaned
}
