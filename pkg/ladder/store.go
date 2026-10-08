package ladder

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// Store is an append-only JSONL event ledger.
type Store struct{ path string }

// DefaultStorePath resolves ladder state beneath BASHY_HOME or ~/.bashy.
func DefaultStorePath() string {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err == nil {
			home = filepath.Join(userHome, ".bashy")
		} else {
			home = ".bashy"
		}
	}
	return filepath.Join(home, "ladder", "events.jsonl")
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = DefaultStorePath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

func (s *Store) Append(e Event) error {
	if s == nil {
		return errors.New("ladder: nil store")
	}
	if !eventKnownKind(e.Kind) || e.Agent == "" || e.Season < 1 || (e.Kind == EventKindDelivery && !ValidPoints(e.Points)) {
		return errors.New("ladder: invalid event")
	}
	if e.Schema == "" {
		e.Schema = EventSchema
	}
	if e.Schema != EventSchema {
		return errors.New("ladder: invalid schema")
	}
	lock, err := lockfile.Acquire(s.path+".lock", lockfile.Holder{
		Name: "ladder", PID: os.Getpid(), Intent: "append ladder event", Since: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("ladder: lock: %w", err)
	}
	defer lock.Release()
	// A producer retry repeats the same immutable event ID. Treat it as a
	// successful append so restart/resume cannot double-count evidence. An
	// empty ID retains the legacy append-every-call behavior.
	if e.ID != "" {
		events, readErr := s.Read()
		if readErr != nil {
			return readErr
		}
		for _, existing := range events {
			if existing.ID == e.ID {
				if sameEvent(existing, e) {
					return nil
				}
				return fmt.Errorf("ladder: event id %q conflicts with existing immutable event", e.ID)
			}
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func sameEvent(a, b Event) bool {
	if a.Schema == "" {
		a.Schema = EventSchema
	}
	if b.Schema == "" {
		b.Schema = EventSchema
	}
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && string(ab) == string(bb)
}

// Read returns valid events. A malformed final unterminated line is reported with the valid prefix.
func (s *Store) Read() ([]Event, error) {
	if s == nil {
		return nil, errors.New("ladder: nil store")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var events []Event
	line := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF && len(raw) == 0 {
			return events, nil
		}
		if readErr != nil && readErr != io.EOF {
			return events, readErr
		}
		line++
		var e Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
			if readErr == io.EOF {
				return events, fmt.Errorf("ladder: truncated last line %d: %w", line, err)
			}
			return events, fmt.Errorf("ladder: invalid line %d: %w", line, err)
		}
		events = append(events, e)
		if readErr == io.EOF {
			return events, nil
		}
	}
}
