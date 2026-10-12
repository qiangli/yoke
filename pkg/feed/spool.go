package feed

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

const spoolFile = "spool.jsonl"

// SpoolEntry is one fetched post. The spool is append-only: Seq increases
// monotonically and nothing is rewritten, so a changed post is a NEW entry.
type SpoolEntry struct {
	Seq      int64    `json:"seq"`
	ID       string   `json:"id"`
	Source   string   `json:"source"`
	Rel      string   `json:"rel"`
	Hash     string   `json:"hash"` // sha256 of the fetched bytes
	Status   string   `json:"status"`
	Detail   string   `json:"detail,omitempty"`
	Fetched  string   `json:"fetched"`
	Author   string   `json:"author,omitempty"`
	Created  string   `json:"created,omitempty"`
	Headline string   `json:"headline,omitempty"`
	Dek      string   `json:"dek,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	Pubkey   string   `json:"pubkey,omitempty"`
	Body     string   `json:"body,omitempty"`
}

func spoolPath(dir string) string { return filepath.Join(dir, spoolFile) }

// ReadSpool returns every spool entry in seq order. A torn trailing line (a
// crashed append) is skipped.
func ReadSpool(dir string) ([]SpoolEntry, error) {
	f, err := os.Open(spoolPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed: read spool: %w", err)
	}
	defer f.Close()
	var out []SpoolEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var e SpoolEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Seq <= 0 {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("feed: read spool: %w", err)
	}
	return out, nil
}

func rawHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Ingest records the post fetched from source/rel in the spool and returns
// the new entry, or nil when an entry with the same id and content hash is
// already spooled. A known id whose content changed is recorded as a new
// "drifted" entry whatever its signature says: the edit is the news.
func Ingest(dir, source, rel string, raw []byte, now time.Time) (*SpoolEntry, error) {
	p, err := ParsePost(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if p.ID == "" {
		return nil, fmt.Errorf("%s: post has no id", rel)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("feed: spool dir: %w", err)
	}
	lock, err := lockfile.AcquireWithin(filepath.Join(dir, spoolFile+".lock"), 15*time.Second,
		lockfile.Holder{Name: "feed", Intent: "spool append"})
	if err != nil {
		return nil, fmt.Errorf("feed: spool lock: %w", err)
	}
	defer lock.Release()

	existing, err := ReadSpool(dir)
	if err != nil {
		return nil, err
	}
	hash := rawHash(raw)
	var last int64
	known := false
	for _, e := range existing {
		if e.Seq > last {
			last = e.Seq
		}
		if e.ID == p.ID {
			if e.Hash == hash {
				return nil, nil
			}
			known = true
		}
	}
	status, detail := verifyStatus(p)
	if known {
		status, detail = StatusDrifted, "content changed since first fetch"
		if _, d := verifyStatus(p); d != "" {
			detail += "; " + d
		}
	}
	e := SpoolEntry{
		Seq: last + 1, ID: p.ID, Source: source, Rel: rel, Hash: hash,
		Status: status, Detail: detail, Fetched: now.UTC().Format(time.RFC3339),
		Author: p.Author, Created: p.Created, Headline: p.Headline, Dek: p.Dek,
		Labels: p.Labels, ReplyTo: p.ReplyTo, Pubkey: p.Pubkey, Body: p.Body,
	}
	line, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(spoolPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("feed: append spool: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return nil, fmt.Errorf("feed: append spool: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("feed: append spool: %w", err)
	}
	return &e, nil
}
