package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// ResolveAttachmentRef maps one frontmatter attachment path to the
// sink-relative reference to download: the post's own directory joined with
// the listed path. Paths that escape the sink root (or are otherwise unsafe)
// are rejected so a hostile post can never pull a fetch outside its post
// directory.
func ResolveAttachmentRef(postRel, attPath string) (string, error) {
	if attPath == "" {
		return "", fmt.Errorf("feed: attachment path is empty")
	}
	clean, err := validateRelPath(attPath)
	if err != nil {
		return "", fmt.Errorf("feed: attachment %q is not a safe relative path", attPath)
	}
	ref, err := validateRelPath(path.Join(path.Dir(postRel), clean))
	if err != nil {
		return "", fmt.Errorf("feed: attachment %q escapes the post directory", attPath)
	}
	return ref, nil
}

// VerifyPostAttachments downloads every attachment listed by p through src
// and checks its bytes against the listed sha256. It returns "" when the
// post lists nothing or everything matches; otherwise it returns a
// human-readable detail naming each problem (hash mismatch or missing
// file). It never fails the fetch: the caller records the detail as drift.
func VerifyPostAttachments(ctx context.Context, src Source, postRel string, p *Post) string {
	if p == nil || len(p.Attachments) == 0 || src == nil {
		return ""
	}
	var problems []string
	for _, a := range p.Attachments {
		ref, err := ResolveAttachmentRef(postRel, a.Path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%q: %v", a.Path, oneLine(err.Error())))
			continue
		}
		data, err := src.Get(ctx, ref)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%q: missing (%s)", a.Path, oneLine(err.Error())))
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, strings.TrimSpace(a.SHA256)) || strings.TrimSpace(a.SHA256) == "" {
			problems = append(problems, fmt.Sprintf("%q: sha256 mismatch (post lists %.12s)", a.Path, a.SHA256))
			continue
		}
	}
	return strings.Join(problems, "; ")
}

// attachmentCheck returns the spool hook for one fetched post: it verifies
// the post's listed attachments through src, or nil when src is nil (kept
// for callers, such as Ingest, that fetch no attachments).
func attachmentCheck(ctx context.Context, src Source, postRel string) func(*Post) string {
	if src == nil {
		return nil
	}
	return func(p *Post) string {
		return VerifyPostAttachments(ctx, src, postRel, p)
	}
}
