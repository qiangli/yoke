// Package quotaobs stores redacted vendor quota observations without depending
// on the budget, broker, or weave packages.
package quotaobs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/room"
)

const Schema = "bashy-quota-observation-v1"
const maxFile = 5 << 20

// ErrRealHomeWrite reports a test that tried to journal an observation inside
// the default real-home .bashy directory instead of an isolated BASHY_HOME.
var ErrRealHomeWrite = errors.New("quotaobs: refusing test write inside the real home's .bashy")

// realHome is captured at process start, before any test scaffolding redirects
// HOME or BASHY_HOME, so the guard can still recognize the operator's home.
var realHome, realHomeErr = os.UserHomeDir()

// defaultBashyDir is the .bashy directory the journal resolves to with no
// isolation: the real home captured at process init joined with .bashy.
func defaultBashyDir() string {
	if realHomeErr != nil || realHome == "" {
		return ""
	}
	return filepath.Join(realHome, ".bashy")
}

// guardTestWrite refuses journal writes that would land inside the default
// real-home .bashy directory while running under go test: an unfenced test
// fails loudly here instead of silently polluting ~/.bashy/quota. Paths
// merely under HOME are legitimate isolated journals — a builder container
// that sets HOME=/tmp puts t.TempDir() there — and are never refused.
func guardTestWrite(p string) error {
	if !testing.Testing() {
		return nil
	}
	def := defaultBashyDir()
	if def == "" {
		return nil
	}
	rel, err := filepath.Rel(def, p)
	if err != nil {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRealHomeWrite, p)
}

// ParsedSignal is the deterministic interpretation of a vendor message.
type ParsedSignal struct {
	Signal      string     `json:"signal"`
	Kind        string     `json:"kind"`
	Window      string     `json:"window,omitempty"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// Analysis is the model's interpretation, cached by excerpt hash and model.
type Analysis struct {
	ExcerptSHA  string     `json:"excerpt_sha"`
	Kind        string     `json:"kind"`
	Window      string     `json:"window"`
	UsedPercent *float64   `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
	AccountHint string     `json:"account_hint"`
	Confidence  float64    `json:"confidence"`
	Summary     string     `json:"summary"`
}

type Observation struct {
	Schema     string            `json:"schema"`
	At         time.Time         `json:"at"`
	Source     string            `json:"source"`
	Tool       string            `json:"tool,omitempty"`
	Model      string            `json:"model,omitempty"`
	Agent      string            `json:"agent,omitempty"`
	Plan       string            `json:"plan,omitempty"`
	Account    string            `json:"account,omitempty"`
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Excerpt    string            `json:"excerpt"`
	ExcerptSHA string            `json:"excerpt_sha"`
	Parsed     *ParsedSignal     `json:"parsed,omitempty"`
	Analysis   *Analysis         `json:"analysis,omitempty"`
}

type Filter func(Observation) bool

var bearerToken = regexp.MustCompile(`(?i)(\b(?:bearer|api[_ -]?key|token)\s*[:= ]\s*)[A-Za-z0-9_./+\-]{12,}`)
var keyToken = regexp.MustCompile(`\b(?:sk|AIza)[A-Za-z0-9_\-]{16,}\b`)

func sanitize(o Observation) Observation {
	o.Schema = Schema
	if o.At.IsZero() {
		o.At = time.Now()
	}
	o.Excerpt = redact.FromHost().String(o.Excerpt)
	o.Excerpt = bearerToken.ReplaceAllString(o.Excerpt, "${1}[REDACTED]")
	o.Excerpt = keyToken.ReplaceAllString(o.Excerpt, "[REDACTED]")
	if len(o.Excerpt) > 2048 {
		o.Excerpt = o.Excerpt[:2048]
	}
	sum := sha256.Sum256([]byte(o.Excerpt))
	o.ExcerptSHA = hex.EncodeToString(sum[:])
	clean := make(map[string]string)
	for k, v := range o.Headers {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "retry-after" && !strings.HasPrefix(k, "x-ratelimit-") && !strings.HasPrefix(k, "anthropic-ratelimit-") {
			continue
		}
		if len(v) > 128 {
			v = v[:128]
		}
		clean[k] = v
	}
	o.Headers = clean
	return o
}

func path() (string, error) {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".bashy")
	}
	return filepath.Join(home, "quota", "observations.jsonl"), nil
}

func Record(ctx context.Context, o Observation) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := guardTestWrite(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(p), 0700); err != nil {
		return err
	}
	lock, err := acquire(ctx, p+".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	o = sanitize(o)
	prior, err := Read(time.Time{}, nil)
	if err != nil {
		return err
	}
	cutoff := o.At.Add(-10 * time.Minute)
	for _, old := range prior {
		if old.ExcerptSHA == o.ExcerptSHA && old.Account == o.Account && old.Source == o.Source && old.Tool == o.Tool && old.Model == o.Model && old.Agent == o.Agent && old.Plan == o.Plan && old.Status == o.Status && maps.Equal(old.Headers, o.Headers) && !old.At.Before(cutoff) && !old.At.After(o.At) {
			return nil
		}
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if st, err := os.Stat(p); err == nil && st.Size()+int64(len(b)+1) > maxFile {
		if err := os.Rename(p, p+".1"); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// The journal remains the durable source if the room is unavailable. The
	// event carries the same sanitized record for --since consumers.
	if !testing.Testing() || os.Getenv("BASHY_ROOM_DIR") != "" {
		_ = room.Emit(room.Event{Type: room.EventUsage, Actor: "quotaobs", Topic: room.TopicQuotaObservation, Body: string(b)})
	}
	return nil
}

func acquire(ctx context.Context, p string) (*lockfile.Lock, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, err := lockfile.AcquireWithin(p, 20*time.Millisecond, lockfile.Holder{Name: "quotaobs", Intent: "observation append"})
		if !errors.Is(err, lockfile.ErrHeld) || !time.Now().Before(deadline) {
			return l, err
		}
	}
}

// Read streams the active journal and its one rotated predecessor.
func Read(since time.Time, filter Filter) ([]Observation, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	var out []Observation
	for _, name := range []string{p + ".1", p} {
		got, err := readFile(name, since, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func readFile(p string, since time.Time, filter Filter) ([]Observation, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var out []Observation
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		} // Incomplete final append is retried after its writer finishes.
		if err != nil {
			return nil, err
		}
		var o Observation
		if err := json.Unmarshal(line, &o); err != nil {
			return nil, err
		}
		if !o.At.Before(since) && (filter == nil || filter(o)) {
			out = append(out, o)
		}
	}
	return out, nil
}
