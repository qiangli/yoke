package kb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/kblife"
)

// Lifecycle wiring (design §7): kb pages carry a validity interval and may be
// forgotten by a tombstone in the ring's journal. pkg/kblife owns the
// semantics; this file only maps pages and journal lines onto it. Nothing is
// deleted — a forgotten page keeps its file, and readers resolve visibility at
// their as-of instant.

// Validity is the page's interval as kblife sees it. An unparseable bound
// reads as open: kb reads the world's pages, it does not lint them.
func (p *Page) Validity() kblife.Validity {
	return kblife.Validity{ValidFrom: parseRFC3339(p.ValidFrom), ValidTo: parseRFC3339(p.ValidTo)}
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// supersededMarker is what Resolve sees as "superseded by": the successor's
// slug, or the status itself for a page flipped by `update --status
// superseded` with no replacement to name.
func (p *Page) supersededMarker() string {
	if p.SupersededBy != "" {
		return p.SupersededBy
	}
	if p.Status == StatusSuperseded {
		return StatusSuperseded
	}
	return ""
}

// PageState resolves p at asOf. tomb is p's tombstone, nil when not forgotten.
func PageState(p *Page, tomb *kblife.Tombstone, asOf time.Time) kblife.State {
	return kblife.Resolve(p.Validity(), p.supersededMarker(), tomb, asOf)
}

// forgetRecord is the journal line `kb forget` appends: an ordinary journal
// record (op "forget") plus the tombstone's ref/reason/by. Same file, same
// O_APPEND discipline — the journal is the audit trail, not a new store.
type forgetRecord struct {
	journalRecord
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
	By     string `json:"by"`
}

// Tombstones replays the journal's forget records, keyed by page slug. The
// first tombstone for a slug wins — forgetting is not re-dated.
func (s *Store) Tombstones() (map[string]kblife.Tombstone, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	out := map[string]kblife.Tombstone{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec forgetRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Op != "forget" || rec.Slug == "" {
			continue
		}
		if _, seen := out[rec.Slug]; seen {
			continue
		}
		out[rec.Slug] = kblife.Tombstone{Ref: rec.Ref, Reason: rec.Reason, By: rec.By, Episode: rec.Episode, At: rec.At}
	}
	return out, sc.Err()
}

// Tombstone returns p's tombstone, or nil when p is not forgotten.
func (s *Store) Tombstone(slug string) (*kblife.Tombstone, error) {
	tombs, err := s.Tombstones()
	if err != nil {
		return nil, err
	}
	if t, ok := tombs[slug]; ok {
		return &t, nil
	}
	return nil, nil
}

// Forget writes p's tombstone to the journal. The page file is never touched.
func (s *Store) Forget(p *Page, reason string, at time.Time) (kblife.Tombstone, error) {
	if prior, err := s.Tombstone(p.Slug); err != nil {
		return kblife.Tombstone{}, err
	} else if prior != nil {
		return kblife.Tombstone{}, fmt.Errorf("kb: %s already forgotten at %s", p.Slug, prior.At.Format(time.RFC3339))
	}
	tomb, err := kblife.NewTombstone("kb:"+p.Slug, reason, ToolID(), EpisodeID(), at.UTC())
	if err != nil {
		return kblife.Tombstone{}, err
	}
	rec := forgetRecord{
		journalRecord: journalRecord{Op: "forget", Slug: p.Slug, ID: p.ID, Tool: tomb.By, Host: s.hostID(),
			Episode: tomb.Episode, ParentEpisode: ParentEpisodeID(), At: tomb.At},
		Ref: tomb.Ref, Reason: tomb.Reason, By: tomb.By,
	}
	if err := s.appendJournal(rec); err != nil {
		return kblife.Tombstone{}, err
	}
	if err := s.RebuildIndex(); err != nil {
		return kblife.Tombstone{}, err
	}
	s.gitSnapshot("kb: forget " + p.Slug)
	return tomb, nil
}

// appendJournal appends one JSON line to the ring's journal and reports the
// error — for records where the line IS the operation (forget, observe).
func (s *Store) appendJournal(v any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// HideForgotten drops pages forgotten at or before asOf.
func HideForgotten(pages []*Page, tombs map[string]kblife.Tombstone, asOf time.Time) []*Page {
	if len(tombs) == 0 {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if t, ok := tombs[p.Slug]; ok && !t.At.After(asOf) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// VisibleAt keeps the pages whose resolved state at asOf is valid — the
// default recall view: superseded, expired, forgotten and not-yet-valid pages
// drop out, and a page superseded AFTER asOf is back in.
func VisibleAt(pages []*Page, tombs map[string]kblife.Tombstone, asOf time.Time) []*Page {
	out := pages[:0:0]
	for _, p := range pages {
		var tomb *kblife.Tombstone
		if t, ok := tombs[p.Slug]; ok {
			tomb = &t
		}
		if kblife.Visible(PageState(p, tomb, asOf)) {
			out = append(out, p)
		}
	}
	return out
}

// --- forget --------------------------------------------------------------

func newForgetCmd(dir, ring *string) *cobra.Command {
	var reason string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "forget <slug> --reason R",
		Short: "Hide a page from search/recall/context with a tombstone (nothing is deleted)",
		Long: `Write a tombstone for the page to the ring's journal. The page file stays;
search, recall and kb context stop returning it, kb show still opens it (with
its lifecycle state), and kb log keeps the tombstone — who, when, why.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("kb: forget needs --reason")
			}
			store := openRing(*dir, *ring)
			p, err := store.LoadByHandle(strings.TrimPrefix(args[0], "kb:"))
			if err != nil {
				return err
			}
			tomb, err := store.Forget(p, reason, time.Now())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(tomb)
			}
			fmt.Fprintf(c.OutOrStdout(), "forgot %s (%s)\n", p.Slug, tomb.Reason)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why this page is forgotten (required; kept in the audit trail)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the tombstone as JSON")
	return cmd
}
