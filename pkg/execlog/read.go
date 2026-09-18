// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Coverage is what the corpus can and cannot answer.
//
// Every read verb must render this. An empty result set has at least four
// different causes — nothing matched, recording was off, the days that held
// the answer were pruned, or the records died unflushed — and they are
// indistinguishable without it. Reporting "no failures found" when the real
// answer is "the evidence was deleted last Tuesday" is the exact shape of
// failure this codebase keeps producing: a conclusion reached by the ABSENCE
// of evidence.
type Coverage struct {
	Records   int       `json:"records"`
	Days      int       `json:"days"`
	From      time.Time `json:"from,omitempty"`
	To        time.Time `json:"to,omitempty"`
	Malformed int       `json:"malformed,omitempty"`
	Lost      int       `json:"lost,omitempty"`   // seq gaps: records that died before flush
	Pruned    int       `json:"pruned,omitempty"` // from tombstones
	Recording bool      `json:"recording"`
}

// Query selects records.
type Query struct {
	Episode string
	Cmd     string
	Since   time.Time
	Limit   int
	Failed  bool
}

// Read returns matching records in space-time order, plus what the corpus
// could not tell you.
func Read(root string, q Query) ([]Record, Coverage, error) {
	cov := Coverage{}

	days, err := dayDirs(root)
	if err != nil {
		return nil, cov, err
	}
	cov.Days = len(days)
	cov.Pruned = prunedCount(root)

	var out []Record
	seen := map[string]map[int]*seqSpan{} // episode -> pid -> span

	for _, day := range days {
		files, _ := filepath.Glob(filepath.Join(day, "*.jsonl"))
		for _, path := range files {
			if strings.HasSuffix(path, pruneFile) {
				continue
			}
			recs, bad := readFile(path)
			cov.Malformed += bad
			for _, r := range recs {
				cov.Records++
				trackSeq(seen, r)
				if !matches(r, q) {
					continue
				}
				out = append(out, r)
			}
		}
	}

	cov.Lost = lostFrom(seen)
	sortRecords(out)

	if !cov.From.IsZero() || len(out) > 0 {
		cov.From, cov.To = span(out)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, cov, nil
}

// seqSpan tracks what one process claimed to have written.
type seqSpan struct {
	min, max uint64
	n        int
}

func trackSeq(seen map[string]map[int]*seqSpan, r Record) {
	byPID, ok := seen[r.Episode]
	if !ok {
		byPID = map[int]*seqSpan{}
		seen[r.Episode] = byPID
	}
	s, ok := byPID[r.PID]
	if !ok {
		s = &seqSpan{min: r.Seq, max: r.Seq}
		byPID[r.PID] = s
	}
	if r.Seq < s.min {
		s.min = r.Seq
	}
	if r.Seq > s.max {
		s.max = r.Seq
	}
	s.n++
}

// lostFrom counts records a process stamped but never landed.
//
// This is the whole reason Seq is assigned at creation rather than at flush:
// it converts an invisible loss into a countable one.
func lostFrom(seen map[string]map[int]*seqSpan) int {
	lost := 0
	for _, byPID := range seen {
		for _, s := range byPID {
			if span := int(s.max-s.min) + 1; span > s.n {
				lost += span - s.n
			}
		}
	}
	return lost
}

func matches(r Record, q Query) bool {
	if q.Episode != "" && r.Episode != q.Episode {
		return false
	}
	if q.Cmd != "" && r.Cmd != q.Cmd {
		return false
	}
	if !q.Since.IsZero() && r.At.Before(q.Since) {
		return false
	}
	if q.Failed && (r.Exit == nil || *r.Exit == 0) {
		return false
	}
	return true
}

// sortRecords orders by episode, then by the process's own sequence.
//
// Wall time is NOT the sort key across processes. Two shells sharing an
// inherited episode interleave in the file, and ordering their records by
// timestamp would present the merge as one causal chain. Ordering within a pid
// is the only ordering that means anything.
func sortRecords(rs []Record) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Episode != b.Episode {
			return a.Episode < b.Episode
		}
		if a.PID != b.PID {
			return a.PID < b.PID
		}
		return a.Seq < b.Seq
	})
}

func span(rs []Record) (from, to time.Time) {
	for _, r := range rs {
		if from.IsZero() || r.At.Before(from) {
			from = r.At
		}
		if to.IsZero() || r.At.After(to) {
			to = r.At
		}
	}
	return from, to
}

func readFile(path string) ([]Record, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	var out []Record
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxArgv*4)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// Counted, never silently skipped. A corrupt line is evidence the
			// store was damaged, and swallowing it makes the corpus look whole.
			bad++
			continue
		}
		out = append(out, r)
	}
	if sc.Err() != nil {
		bad++
	}
	return out, bad
}

func dayDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && isDayName(e.Name()) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func isDayName(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
