package feed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func TestParsePostAndMarshalRoundTrip(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	tests := []struct {
		name string
		post *Post
	}{
		{
			name: "full post with all fields",
			post: &Post{
				ID:         "0192d6e3-1234-7abc-9def-0123456789ab",
				Author:     "user_42",
				Created:    "2026-10-12T00:00:00Z",
				Headline:   "Fed Holds Rates Steady",
				Dek:        "Policy makers signal wait-and-see posture into autumn.",
				Labels:     []string{"fed", "macro", "rates"},
				ReplyTo:    "post_0001",
				Visibility: "public",
				Pubkey:     "MCowBQYDK2VwAyEA0123456789abcdef0123456789abcdef0123456789a=",
				Sig:        "MEQCIBxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
				Body:       "# Market Update\n\nFull analysis and charts below.\n\n- Key stat 1\n- Key stat 2",
			},
		},
		{
			name: "minimal post with no optional fields",
			post: &Post{
				Headline: "Short Note",
				Dek:      "Just a quick thought",
				Body:     "Single line body text.",
			},
		},
		{
			name: "empty body",
			post: &Post{
				ID:       "post-empty-body",
				Author:   "author-1",
				Created:  "2026-10-12T01:00:00Z",
				Headline: "Headline without body",
				Dek:      "Summary of link",
				Body:     "",
			},
		},
		{
			name: "body with leading and trailing whitespace",
			post: &Post{
				ID:       "post-trimmed-body",
				Headline: "Headline with untrimmed body",
				Dek:      "Trimming preserves inner content",
				Body:     "\n\n  Indented and spaced content\nSecond line.\n\n",
			},
		},
		{
			name: "multilingual post with CJK and full-width forms",
			post: &Post{
				ID:       "cjk-post-1",
				Author:   "李强",
				Created:  "2026-10-12T02:00:00Z",
				Headline: "央行维持利率不变",
				Dek:      "政策制定者表示将继续观察秋季经济指标。市場動向に注目。",
				Labels:   []string{"经济", "金利", "거시경제"},
				Body:     "# 分析报告\n\n利率决策符合市场预期。\n\n- 第一点：通胀降温\n- 第二点：就业强劲",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marshaled1, err := tt.post.Marshal()
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}

			parsed, err := ParsePost(marshaled1)
			if err != nil {
				t.Fatalf("ParsePost failed: %v", err)
			}

			marshaled2, err := parsed.Marshal()
			if err != nil {
				t.Fatalf("second Marshal failed: %v", err)
			}

			if !bytes.Equal(marshaled1, marshaled2) {
				t.Fatalf("round-trip marshal is not byte-stable:\nFirst:\n%s\nSecond:\n%s", string(marshaled1), string(marshaled2))
			}

			// Validate field contents
			if parsed.ID != tt.post.ID {
				t.Errorf("ID mismatch: got %q, want %q", parsed.ID, tt.post.ID)
			}
			if parsed.Author != tt.post.Author {
				t.Errorf("Author mismatch: got %q, want %q", parsed.Author, tt.post.Author)
			}
			if parsed.Created != tt.post.Created {
				t.Errorf("Created mismatch: got %q, want %q", parsed.Created, tt.post.Created)
			}
			if parsed.Headline != tt.post.Headline {
				t.Errorf("Headline mismatch: got %q, want %q", parsed.Headline, tt.post.Headline)
			}
			if parsed.Dek != tt.post.Dek {
				t.Errorf("Dek mismatch: got %q, want %q", parsed.Dek, tt.post.Dek)
			}
			if !reflect.DeepEqual(parsed.Labels, tt.post.Labels) && (len(parsed.Labels) != 0 || len(tt.post.Labels) != 0) {
				t.Errorf("Labels mismatch: got %v, want %v", parsed.Labels, tt.post.Labels)
			}
			if parsed.ReplyTo != tt.post.ReplyTo {
				t.Errorf("ReplyTo mismatch: got %q, want %q", parsed.ReplyTo, tt.post.ReplyTo)
			}
			if parsed.Visibility != tt.post.Visibility {
				t.Errorf("Visibility mismatch: got %q, want %q", parsed.Visibility, tt.post.Visibility)
			}
			if parsed.Pubkey != tt.post.Pubkey {
				t.Errorf("Pubkey mismatch: got %q, want %q", parsed.Pubkey, tt.post.Pubkey)
			}
			if parsed.Sig != tt.post.Sig {
				t.Errorf("Sig mismatch: got %q, want %q", parsed.Sig, tt.post.Sig)
			}

			wantBody := strings.TrimSpace(tt.post.Body)
			if parsed.Body != wantBody {
				t.Errorf("Body mismatch: got %q, want %q", parsed.Body, wantBody)
			}

			if tt.post.BodySHA256() != parsed.BodySHA256() {
				t.Errorf("BodySHA256 mismatch: orig %q, parsed %q", tt.post.BodySHA256(), parsed.BodySHA256())
			}
		})
	}
}

func TestParsePostErrors(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	tests := []struct {
		name       string
		input      string
		errContain string
	}{
		{
			name:       "empty input",
			input:      "",
			errContain: "no frontmatter block",
		},
		{
			name:       "plain text without frontmatter",
			input:      "This is just markdown text with no frontmatter at all.",
			errContain: "no frontmatter block",
		},
		{
			name:       "fence without newline",
			input:      "---headline: test\n---\n",
			errContain: "no frontmatter block",
		},
		{
			name:       "unterminated frontmatter no closing fence",
			input:      "---\nid: 1\nheadline: test\nbody line\n",
			errContain: "unterminated frontmatter block",
		},
		{
			name:       "unterminated frontmatter with partial fence",
			input:      "---\nid: 1\n--\n",
			errContain: "unterminated frontmatter block",
		},
		{
			name:       "invalid yaml inside frontmatter",
			input:      "---\n: [invalid : yaml\n---\n\nbody\n",
			errContain: "frontmatter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePost([]byte(tt.input))
			if err == nil {
				t.Fatalf("ParsePost succeeded, want error containing %q", tt.errContain)
			}
			if !strings.Contains(err.Error(), tt.errContain) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errContain)
			}
		})
	}
}

func TestBodySHA256Normalization(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	tests := []struct {
		name    string
		body    string
		wantHex string
	}{
		{
			name:    "empty body",
			body:    "",
			wantHex: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:    "whitespace only body",
			body:    "   \n\t\r\n   ",
			wantHex: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name: "trimmed body normalization matches trimmed literal",
			body: "  \n\nhello world\n\n  ",
			// sha256("hello world")
			wantHex: func() string {
				h := sha256.Sum256([]byte("hello world"))
				return hex.EncodeToString(h[:])
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Post{Body: tt.body}
			got := p.BodySHA256()
			if got != tt.wantHex {
				t.Errorf("BodySHA256() = %q, want %q", got, tt.wantHex)
			}
		})
	}

	// Nil receiver safety
	var nilPost *Post
	gotNil := nilPost.BodySHA256()
	emptySHA := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if gotNil != emptySHA {
		t.Errorf("nil Post BodySHA256() = %q, want %q", gotNil, emptySHA)
	}
}

func TestWindowsCRLFLineEndings(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	input := "---\r\nid: test-crlf\r\nheadline: CRLF Test\r\n---\r\n\r\nBody text with CRLF\r\n"
	p, err := ParsePost([]byte(input))
	if err != nil {
		t.Fatalf("ParsePost failed on CRLF: %v", err)
	}
	if p.ID != "test-crlf" {
		t.Errorf("ID = %q, want %q", p.ID, "test-crlf")
	}
	if p.Headline != "CRLF Test" {
		t.Errorf("Headline = %q, want %q", p.Headline, "CRLF Test")
	}
	if p.Body != "Body text with CRLF" {
		t.Errorf("Body = %q, want %q", p.Body, "Body text with CRLF")
	}
}
