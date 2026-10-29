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
	SchemaVersion int      `json:"schema_version"`
	Records       []record `json:"records"`
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
func (s *Store) mutate(fn func(*document) error) error {
	if s.Path == "" {
		return errors.New("accounts: home unavailable")
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	lock, err := lockfile.AcquireWithin(s.Path+".lock", 30*time.Second, lockfile.Holder{Name: "accounts", PID: os.Getpid(), Intent: "update account inventory", Since: time.Now()})
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
func identity(adapter string, o Observation) (string, Status, error) {
	a := o.Account
	if err := validateAccount(a); err != nil {
		return "", "", err
	}
	if adapter == "" {
		return "", "", errors.New("accounts: adapter required")
	}
	base := string(a.Category) + "\x00" + strings.ToLower(a.Provider) + "\x00"
	if a.Category == HostLogin {
		if a.Username != "" && len(a.Connections) > 0 && a.Connections[0].Endpoint != "" {
			return base + "host\x00" + a.Connections[0].Endpoint + "\x00" + a.Username, Resolved, nil
		}
	} else if a.ProviderAccountID != "" {
		// A profile is a selectable identity for AWS, even if profiles share an account number.
		if a.Category == AWS && len(a.Connections) > 0 && a.Connections[0].Profile != "" {
			return base + "id\x00" + a.ProviderAccountID + "\x00" + a.Connections[0].Profile, Resolved, nil
		}
		return base + "id\x00" + a.ProviderAccountID, Resolved, nil
	}
	if o.SourceKey == "" {
		return "", "", errors.New("accounts: unresolved observation needs source key")
	}
	return base + "unresolved\x00" + adapter + "\x00" + o.SourceKey, Unresolved, nil
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
		key := "manual\x00" + strings.ToLower(a.Alias)
		for _, r := range d.Records {
			if r.Key == key {
				return errors.New("accounts: account already exists")
			}
		}
		a.ID = localID(key)
		if a.Status == "" {
			a.Status = Unresolved
			if a.Category == HostLogin && a.Username != "" && len(a.Connections) == 1 && a.Connections[0].Endpoint != "" && a.Connections[0].CredentialRef != "" {
				a.Status = Resolved
			}
		}
		a.Provenance = "manual"
		added = a
		d.Records = append(d.Records, record{Key: key, Observation: a})
		return checkAliases(d)
	})
	return added, err
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
