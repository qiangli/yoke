// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package state owns the per-app, per-web-principal state and private data
// plane used by shaped applications.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// StateSchemaV1 is the on-disk state document version.
	StateSchemaV1 = "bashy-shape-state-v1"
	// AppDataEnv is the environment variable given to an app for its private,
	// principal-specific data directory.
	AppDataEnv = "BASHY_APP_DATA"
)

// Adopted records one accepted feature offering and revision.
type Adopted struct {
	ID  string `json:"id"`
	Rev string `json:"rev"`
}

// Notice is a principal-private shaping notification.
type Notice struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	Message  string `json:"message,omitempty"`
}

// State is the atomic activation record for one (app, principal) pair.
type State struct {
	Schema         string    `json:"schema"`
	ActiveRevision string    `json:"active_revision,omitempty"`
	LastKnownGood  string    `json:"last_known_good,omitempty"`
	Adopted        []Adopted `json:"adopted,omitempty"`
	Notices        []Notice  `json:"notices,omitempty"`
}

// Store addresses exactly one app and one authenticated principal.
type Store struct {
	dir string
}

// Home returns the Bashy state home. It never uses the working directory as a
// fallback, so app data cannot accidentally land in a source checkout.
func Home() (string, error) {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Abs(h)
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", errors.New("shape: cannot resolve the Bashy home")
	}
	return filepath.Join(h, ".bashy"), nil
}

// Open opens the store under $BASHY_HOME/shape.
func Open(app, principal string) (*Store, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	return OpenAt(home, app, principal)
}

// OpenAt opens a store under home. It is the deterministic test and embedding
// seam; production callers should normally use Open.
func OpenAt(home, app, principal string) (*Store, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("shape: empty Bashy home")
	}
	a, err := encodeComponent("app", app)
	if err != nil {
		return nil, err
	}
	p, err := encodeComponent("principal", principal)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("shape: resolve home: %w", err)
	}
	return &Store{dir: filepath.Join(abs, "shape", a, p)}, nil
}

// Dir returns this pair's private directory.
func (s *Store) Dir() string { return s.dir }

// StatePath returns the atomic record path.
func (s *Store) StatePath() string { return filepath.Join(s.dir, "state.json") }

// DataDir creates and returns the app's private data directory.
func (s *Store) DataDir() (string, error) {
	p := filepath.Join(s.dir, "data")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", fmt.Errorf("shape: create data directory: %w", err)
	}
	if err := os.Chmod(p, 0o700); err != nil {
		return "", fmt.Errorf("shape: protect data directory: %w", err)
	}
	return p, nil
}

// Env returns the one environment assignment an app receives for private data.
func (s *Store) Env() ([]string, error) {
	dir, err := s.DataDir()
	if err != nil {
		return nil, err
	}
	return []string{AppDataEnv + "=" + dir}, nil
}

// Read reads the state record. A pair with no record has base-only state.
func (s *Store) Read() (State, error) {
	b, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{Schema: StateSchemaV1}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("shape: read state: %w", err)
	}
	var st State
	decErr := json.Unmarshal(b, &st)
	if decErr != nil {
		return State{}, fmt.Errorf("shape: decode state: %w", decErr)
	}
	if err := validateState(st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Write atomically replaces state.json in its own directory.
func (s *Store) Write(st State) error {
	if st.Schema == "" {
		st.Schema = StateSchemaV1
	}
	if err := validateState(st); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("shape: encode state: %w", err)
	}
	b = append(b, '\n')
	return atomicWrite(s.StatePath(), b, 0o600)
}

// WriteData atomically replaces one relative file beneath BASHY_APP_DATA.
func (s *Store) WriteData(name string, data []byte) error {
	dir, err := s.DataDir()
	if err != nil {
		return err
	}
	p, err := confinedPath(dir, name)
	if err != nil {
		return err
	}
	return atomicWrite(p, data, 0o600)
}

// ReadData reads one relative file beneath BASHY_APP_DATA.
func (s *Store) ReadData(name string) ([]byte, error) {
	dir, err := s.DataDir()
	if err != nil {
		return nil, err
	}
	p, err := confinedPath(dir, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func validateState(st State) error {
	if st.Schema != StateSchemaV1 {
		return fmt.Errorf("shape: unsupported state schema %q", st.Schema)
	}
	seen := map[string]bool{}
	for _, a := range st.Adopted {
		if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.Rev) == "" {
			return errors.New("shape: adopted item needs id and revision")
		}
		if seen[a.ID] {
			return fmt.Errorf("shape: duplicate adopted item %q", a.ID)
		}
		seen[a.ID] = true
	}
	return nil
}

func encodeComponent(kind, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("shape: empty %s", kind)
	}
	var b strings.Builder
	const hex = "0123456789ABCDEF"
	for _, r := range []byte(value) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteByte(r)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[r>>4])
		b.WriteByte(hex[r&15])
	}
	if b.Len() > 240 {
		return "", fmt.Errorf("shape: %s is too long", kind)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("shape: %s contains a control character", kind)
		}
	}
	return b.String(), nil
}

func confinedPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", errors.New("shape: data path must be relative")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("shape: data path escapes its private directory")
	}
	return filepath.Join(root, clean), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("shape: create private directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("shape: protect private directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".shape-*")
	if err != nil {
		return fmt.Errorf("shape: create temporary file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if retErr != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("shape: replace %s: %w", filepath.Base(path), err)
	}
	return nil
}
