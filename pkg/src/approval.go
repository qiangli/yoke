package src

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ApprovalRecord authorizes a protected-path merge: it names the repo and
// the manifest paths a human (or policy) reviewed. pkg/src only reads
// these records — bashy's approve verb writes them through WriteApproval.
// Core paths are never authorized by any approval.
type ApprovalRecord struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Paths    []string `json:"paths"`
	Approver string   `json:"approver"`
	Reason   string   `json:"reason"`
	Time     string   `json:"time"`
}

// approvalsDir is $BASHY_HOME/src/.approvals/.
func approvalsDir(home string) string {
	if home == "" {
		home = Home()
	}
	return filepath.Join(SrcDir(home), ".approvals")
}

// agentName reports the calling agent for authorship fields.
func agentName() string {
	if a := strings.TrimSpace(os.Getenv("BASHY_AGENT")); a != "" {
		return a
	}
	return "unknown"
}

// WriteApproval stores rec for bashy's approve verb and returns its id.
// Paths may name files, directories, or "" for the whole repo. An empty
// Approver defaults to $BASHY_AGENT and an empty Time to now (UTC).
func WriteApproval(home string, rec ApprovalRecord) (string, error) {
	if rec.Repo == "" {
		return "", fmt.Errorf("src: approval needs a repo")
	}
	if len(rec.Paths) == 0 {
		return "", fmt.Errorf("src: approval needs at least one path")
	}
	dir := approvalsDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if rec.Approver == "" {
		rec.Approver = agentName()
	}
	if rec.Time == "" {
		rec.Time = time.Now().UTC().Format(time.RFC3339)
	}
	rec.ID = fmt.Sprintf("%s-%d", rec.Repo, time.Now().UTC().UnixNano())
	rec.Paths = append([]string(nil), rec.Paths...)
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ListApprovals reads every approval record, oldest first, optionally
// filtered to one repo.
func ListApprovals(home, repo string) ([]ApprovalRecord, error) {
	dir := approvalsDir(home)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ApprovalRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec ApprovalRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("src: corrupt approval %s: %w", e.Name(), err)
		}
		if repo != "" && rec.Repo != repo {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// approvalCovers reports whether approval path ap covers diff path p: an
// exact match, p beneath a directory approval, or a whole-repo ("") grant.
func approvalCovers(ap, p string) bool {
	ap = strings.TrimPrefix(p2slash(ap), "./")
	p = strings.TrimPrefix(p2slash(p), "./")
	if ap == "" || ap == "." {
		return true
	}
	return p == ap || strings.HasPrefix(p, ap+"/")
}

func p2slash(p string) string { return filepath.ToSlash(p) }

// approvalsCoverProtected reports whether the stored approvals for repo
// authorize every protected (non-core) path in paths. Core paths are never
// covered: they return the offending paths so the caller can refuse.
func approvalsCoverProtected(home, repo string, paths []string) (covered bool, core []string, err error) {
	var prot []string
	for _, p := range paths {
		switch PathClass(repo, p) {
		case ClassCore:
			core = append(core, p)
		case ClassProtected:
			prot = append(prot, p)
		}
	}
	if len(core) > 0 {
		return false, core, nil
	}
	if len(prot) == 0 {
		return true, nil, nil
	}
	recs, err := ListApprovals(home, repo)
	if err != nil {
		return false, nil, err
	}
	for _, p := range prot {
		ok := false
		for _, r := range recs {
			for _, ap := range r.Paths {
				if approvalCovers(ap, p) {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			return false, nil, nil
		}
	}
	return true, nil, nil
}
