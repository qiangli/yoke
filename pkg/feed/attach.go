package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultMaxAttachBytes caps one attachment when Env.MaxAttachBytes is unset.
const DefaultMaxAttachBytes = 25 << 20 // 25 MiB

// Attachment is one file published beside a post. Path is relative to the
// post's own directory: "<stem>/<basename>", where <stem> is the post file
// name without its extension (so post posts/2026/<id>.md lists
// "<id>/pic.png"); SHA256 is the lowercase hex sha256 of the attachment
// bytes. The signature covers every entry's path and sha256 (see
// SigningPayloadForPost). Fetchers keep accepting any safe relative path,
// so older bare-basename entries still verify.
type Attachment struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// attachmentUpload is a collected attachment ready to publish: its
// frontmatter entry plus the bytes to Put at rel.
type attachmentUpload struct {
	entry Attachment
	rel   string
	data  []byte
}

func (e Env) maxAttachBytes() int64 {
	if e.MaxAttachBytes > 0 {
		return e.MaxAttachBytes
	}
	return DefaultMaxAttachBytes
}

// attachmentBaseName reduces a --attach FILE argument to the name stored
// under the post's own directory. Directory components are dropped (attaching
// /tmp/photo.png stores "photo.png"); anything that cannot become a safe
// single path segment is rejected.
func attachmentBaseName(file string) (string, error) {
	if file == "" {
		return "", fmt.Errorf("feed: attachment path is empty")
	}
	base := filepath.Base(filepath.FromSlash(file))
	if err := validateAttachmentName(base); err != nil {
		return "", fmt.Errorf("feed: attachment %q: %w", file, err)
	}
	return base, nil
}

func validateAttachmentName(base string) error {
	if base == "" || base == "." || base == ".." {
		return fmt.Errorf("invalid attachment name %q", base)
	}
	if strings.ContainsRune(base, 0) {
		return fmt.Errorf("invalid attachment name %q: contains NUL", base)
	}
	if strings.ContainsAny(base, `/\`) {
		return fmt.Errorf("invalid attachment name %q: must be a single file name", base)
	}
	if _, err := validateRelPath(base); err != nil {
		return fmt.Errorf("invalid attachment name %q: %v", base, err)
	}
	return nil
}

// postStem returns the directory name for a post's attachments: the post
// file name without its extension (posts/2026/<id>.md -> <id>,
// posts/x/hello.md -> hello).
func postStem(postRel string) (string, error) {
	stem := strings.TrimSuffix(path.Base(postRel), path.Ext(path.Base(postRel)))
	if err := validateAttachmentName(stem); err != nil {
		return "", fmt.Errorf("feed: post path %q has no safe attachment directory: %w", postRel, err)
	}
	return stem, nil
}

// attachmentRel returns the sink-relative destination for an attachment of
// the post at postRel: the post's own directory joined with the post stem
// and the basename (posts/2026/<id>.md + pic.png ->
// posts/2026/<id>/pic.png). It rejects anything that would collide with
// the post itself or the index, or escape the sink root.
func attachmentRel(postRel, base string) (string, error) {
	if err := validateAttachmentName(base); err != nil {
		return "", err
	}
	if base == path.Base(postRel) {
		return "", fmt.Errorf("feed: attachment %q would overwrite the post itself", base)
	}
	if base == IndexName {
		return "", fmt.Errorf("feed: attachment name %q is reserved for the post index", base)
	}
	stem, err := postStem(postRel)
	if err != nil {
		return "", err
	}
	rel := path.Join(path.Dir(postRel), stem, base)
	rel, err = validateRelPath(rel)
	if err != nil {
		return "", fmt.Errorf("feed: attachment %q escapes the sink root", base)
	}
	if rel == postRel {
		return "", fmt.Errorf("feed: attachment %q would overwrite the post itself", base)
	}
	if rel == IndexName {
		return "", fmt.Errorf("feed: attachment name %q is reserved for the post index", base)
	}
	return rel, nil
}

// collectAttachments reads every --attach FILE, enforcing the per-file size
// cap, safe basenames, and duplicate rejection. It returns the frontmatter
// entries (in flag order) and the blobs to publish.
func collectAttachments(files []string, postRel string, maxBytes int64) ([]Attachment, []attachmentUpload, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachBytes
	}
	seen := map[string]bool{}
	var entries []Attachment
	var uploads []attachmentUpload
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("feed: attachment %q: %w", f, err)
		}
		if int64(len(data)) > maxBytes {
			return nil, nil, fmt.Errorf("feed: attachment %q is %d bytes, over the %d-byte per-attachment limit", f, len(data), maxBytes)
		}
		base, err := attachmentBaseName(f)
		if err != nil {
			return nil, nil, err
		}
		if seen[base] {
			return nil, nil, fmt.Errorf("feed: duplicate attachment name %q", base)
		}
		seen[base] = true
		rel, err := attachmentRel(postRel, base)
		if err != nil {
			return nil, nil, err
		}
		stem, err := postStem(postRel)
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(data)
		entry := Attachment{Path: stem + "/" + base, SHA256: hex.EncodeToString(sum[:])}
		entries = append(entries, entry)
		uploads = append(uploads, attachmentUpload{entry: entry, rel: rel, data: data})
	}
	return entries, uploads, nil
}
