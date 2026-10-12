// Package feed implements the bashy federated feed post format, excerpt caps,
// signing, and sink distribution.
package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Post is one bashy feed entry stored as a kb-Page-style markdown file:
// YAML frontmatter enclosed by "---" fences followed by a markdown body.
type Post struct {
	ID         string   `yaml:"id,omitempty"`
	Author     string   `yaml:"author,omitempty"`
	Created    string   `yaml:"created,omitempty"` // RFC3339
	Headline   string   `yaml:"headline,omitempty"`
	Dek        string   `yaml:"dek,omitempty"`
	Labels     []string `yaml:"labels,omitempty"`
	ReplyTo    string   `yaml:"reply_to,omitempty"`
	Visibility string   `yaml:"visibility,omitempty"`
	Group      string   `yaml:"group,omitempty"`
	Epoch      int      `yaml:"epoch,omitempty"`
	Enc        string   `yaml:"enc,omitempty"`
	Pubkey     string   `yaml:"pubkey,omitempty"`
	Sig        string   `yaml:"sig,omitempty"`
	// Attachments lists files published beside the post, each with its
	// path relative to the post and its sha256 hex digest. The signature
	// covers every entry's path and sha256.
	Attachments []Attachment `yaml:"attachments,omitempty"`

	Body string `yaml:"-"`
}

// ParsePost reads a feed post from its serialized markdown representation:
// a YAML frontmatter block followed by an optional markdown body.
func ParsePost(b []byte) (*Post, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, errors.New("feed: no frontmatter block")
	}
	_, rest, _ := strings.Cut(s, "\n")
	var fm, body string
	if strings.HasPrefix(rest, "---\n") || strings.HasPrefix(rest, "---\r\n") || rest == "---" {
		fm = ""
		body = strings.TrimPrefix(rest, "---")
	} else {
		var ok bool
		fm, body, ok = strings.Cut(rest, "\n---")
		if !ok {
			return nil, errors.New("feed: unterminated frontmatter block")
		}
	}
	var p Post
	if err := yaml.Unmarshal([]byte(fm), &p); err != nil {
		return nil, fmt.Errorf("feed: frontmatter: %w", err)
	}
	// Drop the newline that closed the fence and one blank separator line.
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	p.Body = strings.TrimRight(body, "\r\n")
	return &p, nil
}

// Marshal renders the post to its on-disk markdown format:
// "---\n<yaml frontmatter>\n---\n\n<body>\n".
func (p *Post) Marshal() ([]byte, error) {
	if p == nil {
		return nil, errors.New("feed: nil post")
	}
	fm, err := yaml.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("feed: marshal frontmatter: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n")
	if body := strings.TrimSpace(p.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// BodySHA256 returns the lowercase hex sha256 digest of the body exactly as
// Marshal writes it.
//
// Normalization: leading and trailing whitespace is trimmed using
// strings.TrimSpace, matching kb's page body trimming rule. If the trimmed
// body is empty, it returns the SHA-256 hash of the empty string.
func (p *Post) BodySHA256() string {
	if p == nil {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:])
	}
	trimmed := strings.TrimSpace(p.Body)
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}
