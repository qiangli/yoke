package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

type record struct {
	Key         string   `json:"key"`
	Observation Account  `json:"observation"`
	Override    Override `json:"override,omitempty"`
	Suppressed  bool     `json:"suppressed,omitempty"`
	Bindings    []string `json:"bindings,omitempty"`
}
type document struct {
	SchemaVersion int        `json:"schema_version"`
	Records       []record   `json:"records"`
	Snapshots     []Snapshot `json:"snapshots,omitempty"`
}
type Store struct{ Path string }

func DefaultPath() string {
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".bashy")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "accounts", "accounts.json")
}
func Open(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return &Store{Path: path}
}
func (s *Store) read() (document, error) {
	d := document{SchemaVersion: SchemaVersion, Records: []record{}}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err = json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("accounts: decode: %w", err)
	}
	if d.SchemaVersion > SchemaVersion {
		return d, ErrNewerSchema
	}
	if d.SchemaVersion != SchemaVersion {
		return d, errors.New("accounts: unsupported schema version")
	}
	return d, nil
}

// storeLockWait is how long an ordinary mutation waits for the inventory lock.
// The observation hook passes a much shorter budget: it must never hold up the
// operation that produced the fact.
const storeLockWait = 30 * time.Second

func (s *Store) mutate(fn func(*document) error) error {
	return s.mutateWithin(storeLockWait, fn)
}
func (s *Store) mutateWithin(wait time.Duration, fn func(*document) error) error {
	if s == nil || s.Path == "" {
		return errors.New("accounts: home unavailable")
	}
	if wait <= 0 {
		return errors.New("accounts: no time left to record")
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	lock, err := lockfile.AcquireWithin(s.Path+".lock", wait, lockfile.Holder{Name: "accounts", PID: os.Getpid(), Intent: "update account inventory", Since: time.Now()})
	if err != nil {
		return fmt.Errorf("accounts: lock: %w", err)
	}
	defer lock.Release()
	d, err := s.read()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(d)
	if err = fn(&d); err != nil {
		return err
	}
	sort.Slice(d.Records, func(i, j int) bool { return d.Records[i].Key < d.Records[j].Key })
	sortSnapshots(d.Snapshots)
	after, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	after = append(after, '\n')
	var old document
	if json.Unmarshal(before, &old) == nil {
		oldBytes, _ := json.MarshalIndent(old, "", "  ")
		oldBytes = append(oldBytes, '\n')
		if string(oldBytes) == string(after) {
			return nil
		}
	}
	f, err := os.CreateTemp(dir, ".accounts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(after); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.Path)
}

// identityKey is the one place that decides when a record names a real
// identity by itself: a host login proven by endpoint plus user, or a provider
// account id. Manual records and adapter observations go through it alike, so
// adding an account by hand and later observing the same identity converge on
// one record instead of two.
func identityKey(a Account) (string, bool) {
	base := string(a.Category) + "\x00" + strings.ToLower(a.Provider) + "\x00"
	if a.Category == HostLogin {
		if a.Username != "" && len(a.Connections) > 0 && a.Connections[0].Endpoint != "" {
			return base + "host\x00" + a.Connections[0].Endpoint + "\x00" + a.Username, true
		}
		return "", false
	}
	if a.ProviderAccountID != "" {
		// A profile is a selectable identity for AWS, even if profiles share an account number.
		if a.Category == AWS && len(a.Connections) > 0 && a.Connections[0].Profile != "" {
			return base + "id\x00" + a.ProviderAccountID + "\x00" + a.Connections[0].Profile, true
		}
		return base + "id\x00" + a.ProviderAccountID, true
	}
	return "", false
}

// unresolvedKey names a record that proves no identity of its own. The adapter
// is part of the key because one adapter's source key means nothing in
// another's namespace. Both the discovery path and the observation hook build
// it here: two spellings of this key would split one account into two records.
func unresolvedKey(a Account, adapter, sourceKey string) string {
	return string(a.Category) + "\x00" + strings.ToLower(a.Provider) + "\x00unresolved\x00" + adapter + "\x00" + sourceKey
}

func identity(adapter string, o Observation) (string, Status, error) {
	a := o.Account
	if err := validateAccount(a); err != nil {
		return "", "", err
	}
	if adapter == "" {
		return "", "", errors.New("accounts: adapter required")
	}
	if key, ok := identityKey(a); ok {
		return key, Resolved, nil
	}
	if o.SourceKey == "" {
		return "", "", errors.New("accounts: unresolved observation needs source key")
	}
	return unresolvedKey(a, adapter, o.SourceKey), Unresolved, nil
}
func localID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "acc_" + hex.EncodeToString(sum[:12])
}
func effective(r record) Account {
	a := r.Observation
	if r.Override.Alias != nil {
		a.Alias = *r.Override.Alias
	}
	if r.Override.Name != nil {
		a.Name = *r.Override.Name
	}
	if r.Override.Region != nil {
		a.Region = *r.Override.Region
	}
	if r.Override.Environment != nil {
		a.Environment = *r.Override.Environment
	}
	if r.Override.Links != nil {
		a.Links = *r.Override.Links
	}
	if r.Suppressed {
		a.Status = Suppressed
	}
	return a
}
func checkAliases(d *document) error {
	seen := map[string]bool{}
	for _, r := range d.Records {
		a := strings.ToLower(effective(r).Alias)
		if a != "" {
			if seen[a] {
				return errors.New("accounts: alias already exists")
			}
			seen[a] = true
		}
	}
	return nil
}

// Observe replaces an adapter snapshot only when it changes. It never alters overrides.
func (s *Store) Observe(adapter string, observations []Observation) error {
	return s.mutate(func(d *document) error {
		for _, o := range observations {
			key, status, err := identity(adapter, o)
			if err != nil {
				return err
			}
			a := o.Account
			a.ID = localID(key)
			a.Provenance = adapter
			if a.Status == "" {
				a.Status = status
			}
			if status == Unresolved {
				a.Status = Unresolved
			}
			idx := -1
			for i := range d.Records {
				if d.Records[i].Key == key {
					idx = i
					break
				}
			}
			if idx >= 0 {
				if d.Records[idx].Suppressed {
					continue
				}
				d.Records[idx].Observation = a
			} else {
				d.Records = append(d.Records, record{Key: key, Observation: a})
			}
		}
		return checkAliases(d)
	})
}

// Add creates a local account without claiming that a provider verified it.
// Its ID remains stable when the editable alias changes.
func (s *Store) Add(a Account) (Account, error) {
	if a.Alias == "" {
		return Account{}, errors.New("accounts: manual alias required")
	}
	if err := validateAccount(a); err != nil {
		return Account{}, err
	}
	var added Account
	err := s.mutate(func(d *document) error {
		key, identifiable := identityKey(a)
		if !identifiable {
			// Nothing in the record names an identity an adapter could ever
			// reach, so the alias is the only key available.
			key = "manual\x00" + strings.ToLower(a.Alias)
		}
		for _, r := range d.Records {
			if r.Key == key {
				return &DuplicateIdentityError{Existing: effective(r)}
			}
		}
		a.ID = localID(key)
		if a.Status == "" {
			// Adding by hand is not proof that a provider verified anything;
			// only a fully specified host login is usable as entered.
			a.Status = Unresolved
			if a.Category == HostLogin && a.Username != "" && len(a.Connections) == 1 && a.Connections[0].Endpoint != "" && a.Connections[0].CredentialRef != "" {
				a.Status = Resolved
			}
		}
		a.Provenance = "manual"
		r := record{Key: key}
		if identifiable {
			// An adapter observation of this identity replaces the whole
			// observation, so manual metadata only survives as an override.
			r.Override = manualOverride(a)
			a.Alias = ""
		}
		r.Observation = a
		added = effective(r)
		d.Records = append(d.Records, r)
		return checkAliases(d)
	})
	return added, err
}

// manualOverride lifts the hand-entered fields of an identifiable account into
// the override layer. Everything an adapter can legitimately re-observe stays
// in the observation and is free to change underneath.
func manualOverride(a Account) Override {
	o := Override{Alias: &a.Alias}
	if a.Name != "" {
		name := a.Name
		o.Name = &name
	}
	if a.Region != "" {
		region := a.Region
		o.Region = &region
	}
	if a.Environment != "" {
		env := a.Environment
		o.Environment = &env
	}
	if len(a.Links.Satellites) > 0 || len(a.Links.Hosts) > 0 || len(a.Links.Models) > 0 || len(a.Links.Agents) > 0 {
		links := a.Links
		o.Links = &links
	}
	return o
}
func (s *Store) List() ([]Account, error) {
	d, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(d.Records))
	for _, r := range d.Records {
		out = append(out, effective(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alias != out[j].Alias {
			return out[i].Alias < out[j].Alias
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
func (s *Store) Get(ref string) (Account, error) {
	d, err := s.read()
	if err != nil {
		return Account{}, err
	}
	for _, r := range d.Records {
		a := effective(r)
		if a.ID == ref || a.Alias == ref {
			return a, nil
		}
	}
	return Account{}, ErrNotFound
}
func (s *Store) Override(ref string, o Override) error {
	return s.mutate(func(d *document) error {
		for i := range d.Records {
			a := effective(d.Records[i])
			if a.ID == ref || a.Alias == ref {
				d.Records[i].Override = o
				return checkAliases(d)
			}
		}
		return ErrNotFound
	})
}

// PatchOverride applies CLI-editable metadata without replacing unrelated
// manual overrides. An unset reveals the adapter observation again.
func (s *Store) PatchOverride(ref string, set map[string]string, unset []string) error {
	return s.mutate(func(d *document) error {
		for i := range d.Records {
			a := effective(d.Records[i])
			if a.ID != ref && a.Alias != ref {
				continue
			}
			o := &d.Records[i].Override
			for key, value := range set {
				v := value
				switch key {
				case "alias":
					if v == "" {
						return errors.New("accounts: alias cannot be empty")
					}
					o.Alias = &v
				case "name":
					o.Name = &v
				case "region":
					o.Region = &v
				case "environment":
					o.Environment = &v
				default:
					return errors.New("accounts: invalid editable field")
				}
			}
			for _, key := range unset {
				switch key {
				case "alias":
					o.Alias = nil
				case "name":
					o.Name = nil
				case "region":
					o.Region = nil
				case "environment":
					o.Environment = nil
				default:
					return errors.New("accounts: invalid editable field")
				}
			}
			return checkAliases(d)
		}
		return ErrNotFound
	})
}
func (s *Store) SetBindings(ref string, bindings []string) error {
	return s.mutate(func(d *document) error {
		for i := range d.Records {
			a := effective(d.Records[i])
			if a.ID == ref || a.Alias == ref {
				d.Records[i].Bindings = append([]string(nil), bindings...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// Remove suppresses metadata discovery; bindings must be removed by their owner first.
func (s *Store) Remove(ref string) error {
	return s.mutate(func(d *document) error {
		for i := range d.Records {
			a := effective(d.Records[i])
			if a.ID == ref || a.Alias == ref {
				if len(d.Records[i].Bindings) > 0 {
					return ErrReferenced
				}
				d.Records[i].Suppressed = true
				return nil
			}
		}
		return ErrNotFound
	})
}
func (s *Store) Restore(ref string) error {
	return s.mutate(func(d *document) error {
		for i := range d.Records {
			a := effective(d.Records[i])
			if a.ID == ref || a.Alias == ref {
				d.Records[i].Suppressed = false
				return nil
			}
		}
		return ErrNotFound
	})
}

// Adopt folds a verified identity into the MANUAL record that proved it, so
// `account refresh <alias>` leaves one resolved record rather than the manual
// one plus a new unaliased observation. The record is re-keyed to the identity
// key; the manual alias and overrides survive as overrides. When a record for
// that identity already exists (the observation hook writes one during the
// verify call) the manual alias/overrides merge into it and the manual record
// is removed — no data is lost. The account ID follows the identity key, so
// the manual record's snapshot moves with it. It returns the adopted account.
func (s *Store) Adopt(manualID, adapter string, o Observation) (Account, error) {
	var out Account
	err := s.mutate(func(d *document) error {
		mi := -1
		for i := range d.Records {
			if d.Records[i].Observation.ID == manualID && d.Records[i].Observation.Provenance == "manual" {
				mi = i
				break
			}
		}
		if mi < 0 {
			return ErrNotFound
		}
		key, status, err := identity(adapter, o)
		if err != nil {
			return err
		}
		if status != Resolved {
			return errors.New("accounts: observation proves no identity")
		}
		manual := d.Records[mi]
		if manual.Key == key {
			return ErrNotFound
		}
		ov := manualOverride(effective(manual))
		if manual.Override.Alias != nil {
			ov.Alias = manual.Override.Alias
		}
		a := o.Account
		a.ID = localID(key)
		a.Provenance = adapter
		a.Status = Resolved
		d.Records = append(d.Records[:mi], d.Records[mi+1:]...)
		idx := -1
		for i := range d.Records {
			if d.Records[i].Key == key {
				idx = i
				break
			}
		}
		if idx < 0 {
			d.Records = append(d.Records, record{Key: key, Observation: a, Override: ov, Bindings: manual.Bindings, Suppressed: manual.Suppressed})
			out = effective(d.Records[len(d.Records)-1])
		} else {
			r := &d.Records[idx]
			r.Observation = a
			if ov.Alias != nil {
				r.Override.Alias = ov.Alias
			}
			if ov.Name != nil {
				r.Override.Name = ov.Name
			}
			if ov.Region != nil {
				r.Override.Region = ov.Region
			}
			if ov.Environment != nil {
				r.Override.Environment = ov.Environment
			}
			if ov.Links != nil {
				r.Override.Links = ov.Links
			}
			for _, b := range manual.Bindings {
				dup := false
				for _, x := range r.Bindings {
					dup = dup || x == b
				}
				if !dup {
					r.Bindings = append(r.Bindings, b)
				}
			}
			r.Suppressed = r.Suppressed && manual.Suppressed
			out = effective(*r)
		}
		for i := range d.Snapshots {
			if d.Snapshots[i].AccountID == manualID {
				d.Snapshots[i].AccountID = out.ID
			}
		}
		return checkAliases(d)
	})
	return out, err
}
