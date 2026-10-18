package dblifecycle

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/ref"
)

// ParseTarget consumes the shared S11 locator. The db provider owns only the
// decoded target components; escaping and location syntax stay with pkg/ref.
func ParseTarget(raw string) (dbstore.Target, error) {
	l, err := ref.ParseLocatorWithSchemes(raw, map[string]bool{"db": true})
	if err != nil {
		return dbstore.Target{}, fmt.Errorf("%w: %v", dbstore.ErrInvalidInput, err)
	}
	if l.Scheme != "db" || l.ScopeKind != "" || l.URN || l.Revision != "" || len(l.Inner) != 0 {
		return dbstore.Target{}, fmt.Errorf("%w: expected a local db locator", dbstore.ErrInvalidInput)
	}
	engine, rest, ok := strings.Cut(l.Target, ":")
	if !ok {
		return dbstore.Target{}, fmt.Errorf("%w: db target requires engine and mode", dbstore.ErrInvalidInput)
	}
	mode, value, ok := strings.Cut(rest, ":")
	if !ok {
		return dbstore.Target{}, fmt.Errorf("%w: db target requires mode and value", dbstore.ErrInvalidInput)
	}
	t := dbstore.Target{Engine: engine}
	switch mode {
	case "file":
		t.Mode, t.Path = dbstore.Persistent, value
	case "mem":
		t.Mode = dbstore.Memory
		t.Session, t.Name, ok = strings.Cut(value, "/")
		if !ok {
			return dbstore.Target{}, fmt.Errorf("%w: memory target requires session/name", dbstore.ErrInvalidInput)
		}
	default:
		return dbstore.Target{}, fmt.Errorf("%w: db target mode %q unsupported", dbstore.ErrUnsupported, mode)
	}
	if _, err := dbstore.Resolve(t); err != nil {
		return dbstore.Target{}, err
	}
	return t, nil
}
