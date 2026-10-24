// Package prune defines the small, read-first contract shared by bashy prune nouns.
// Implementations must recheck ownership and liveness in Apply; a dry-run item is
// an observation, never authority to delete a path later.
package prune

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Item is one eligible deletion. Size is apparent bytes, or zero if unknown.
type Item struct {
	Noun   string `json:"noun"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Size   int64  `json:"size"`
}

type Options struct {
	Older time.Duration `json:"older"`
	Apply bool          `json:"apply"`
	JSON  bool          `json:"json"`
}

type Counts struct {
	Removed int   `json:"removed"`
	Bytes   int64 `json:"bytes"`
}

// Noun supplies a read-only candidate pass and guarded removal of those items.
// Apply must recheck each item's eligibility immediately before removal.
type Noun interface {
	Name() string
	DryRun(context.Context, Options) ([]Item, error)
	Apply(context.Context, []Item) (Counts, error)
}

type Registry struct{ nouns map[string]Noun }

func NewRegistry() *Registry { return &Registry{nouns: make(map[string]Noun)} }

func (r *Registry) Register(n Noun) error {
	if n == nil || n.Name() == "" || strings.ContainsAny(n.Name(), " \t/\\") {
		return fmt.Errorf("prune: invalid noun")
	}
	if _, ok := r.nouns[n.Name()]; ok {
		return fmt.Errorf("prune: duplicate noun %q", n.Name())
	}
	r.nouns[n.Name()] = n
	return nil
}

func (r *Registry) Nouns() []Noun {
	out := make([]Noun, 0, len(r.nouns))
	for _, n := range r.nouns {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ParseOlder accepts Go durations plus whole days, e.g. 30d.
func ParseOlder(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseUint(strings.TrimSuffix(s, "d"), 10, 32)
		if err != nil || days == 0 || days > uint64((1<<63-1)/int64(24*time.Hour)) {
			return 0, fmt.Errorf("invalid --older %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --older %q", s)
	}
	return d, nil
}

// Home resolves bashy's managed home without creating it.
func Home() string {
	if s := strings.TrimSpace(os.Getenv("BASHY_HOME")); s != "" {
		return s
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".bashy")
}

// ApparentSize measures regular files without following symlinks.
func ApparentSize(path string) (int64, error) {
	var bytes int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		bytes += st.Size()
		return nil
	})
	return bytes, err
}

func FormatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, unit := range units {
		f /= 1024
		if f < 1024 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return "0 B"
}

// PrintItems gives an operator the reason for every proposed deletion.
func PrintItems(w io.Writer, items []Item, applied bool, counts Counts) {
	verb := "would remove"
	if applied {
		verb = "removed"
	}
	for _, item := range items {
		fmt.Fprintf(w, "%s %s %s (%s): %s\n", verb, item.Noun, item.ID, FormatSize(item.Size), item.Reason)
	}
	if applied {
		fmt.Fprintf(w, "removed %d item(s), %s\n", counts.Removed, FormatSize(counts.Bytes))
	} else {
		fmt.Fprintf(w, "%d item(s), %s; apply with --apply\n", len(items), FormatSize(SumSize(items)))
	}
}

func SumSize(items []Item) int64 {
	var n int64
	for _, item := range items {
		n += item.Size
	}
	return n
}
