package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/fleet"
)

// retiredDirName is the sidecar directory of retirements in the local ring.
// A skill's folder may live in a ring that cannot be edited (embedded, shared)
// and retiring must not copy it — a copy would freeze the original — so a
// retirement is its own small record beside the folders. Folder sources ignore
// dot-directories, so it is never listed as a skill.
const retiredDirName = ".retired"

type retiredStore struct{ dir string }

func (s retiredStore) path(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("skills: invalid skill name %q", name)
	}
	return filepath.Join(s.dir, name+".yaml"), nil
}

func (s retiredStore) get(name string) fleet.RecordLifecycle {
	if s.dir == "" {
		return fleet.RecordLifecycle{}
	}
	p, err := s.path(name)
	if err != nil {
		return fleet.RecordLifecycle{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return fleet.RecordLifecycle{}
	}
	var r fleet.Retirement
	if err := yaml.Unmarshal(data, &r); err != nil {
		// A sidecar that exists but cannot be read still means "retired":
		// failing open would let a retired skill run.
		return fleet.RecordLifecycle{Retired: &fleet.Retirement{Reason: "unreadable retirement record"}}
	}
	return fleet.RecordLifecycle{Retired: &r}
}

func (s retiredStore) set(name string, r *fleet.Retirement) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if r == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Lifecycle reports a skill's retirement, whichever ring defines it.
func (c *Catalog) Lifecycle(name string) fleet.RecordLifecycle {
	return retiredStore{c.RetiredDir}.get(name)
}

// CheckRetired refuses new work (export, run) on a retired skill.
func (c *Catalog) CheckRetired(name string) error {
	return c.Lifecycle(name).CheckRetired(fleet.KindSkill, name)
}

// Storage adapts the catalog to fleet's lifecycle verbs, so skills get the
// same retire/unretire as every other kind without moving off folders.
func (c *Catalog) Storage() fleet.Storage { return skillStorage{c} }

type skillStorage struct{ cat *Catalog }

func (s skillStorage) SetRetirement(name string, r *fleet.Retirement) error {
	if _, _, ok := s.cat.Get(name); !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	if s.cat.RetiredDir == "" {
		return fmt.Errorf("skills: no local store to record a retirement in")
	}
	return retiredStore{s.cat.RetiredDir}.set(name, r)
}
