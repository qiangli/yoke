// Package dbobj implements the Sprint 453 object storage category (story e6da3d7c62e6):
// bucket management, object metadata, bounded listing, get/put/delete, backup transfer,
// and logical import/export under the database tool contracts.
package dbobj

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// Operation names registered with dblifecycle.
const (
	OpBucketCreate = "obj.bucket.create"
	OpBucketDrop   = "obj.bucket.drop"
	OpBucketList   = "obj.bucket.list"
	OpBucketAdd    = "obj.bucket.add" // alias for create
	OpBucketRm     = "obj.bucket.rm"  // alias for drop
	OpList         = "obj.list"
	OpStat         = "obj.stat"
	OpGet          = "obj.get"
	OpPut          = "obj.put"
	OpDelete       = "obj.delete"
	OpRm           = "obj.rm" // alias for delete
	OpTransfer     = "obj.transfer"
	OpExport       = "obj.export"
	OpImport       = "obj.import"
)

// Bounds declared for the object category (contract §6).
const (
	DefaultLimit    = 100
	MaxLimit        = 10000
	DefaultMaxBytes = 256 << 10 // 256 KB
	MaxMaxBytes     = 32 << 20  // 32 MB
	MaxBucketName   = 255
	MaxKeyLength    = 1024
)

// Value holds byte payloads as either valid UTF-8 text or base64 data.
type Value struct {
	Text *string `json:"text,omitempty"`
	B64  *string `json:"b64,omitempty"`
}

// Bytes decodes the value.
func (v *Value) Bytes() ([]byte, error) {
	switch {
	case v == nil || (v.Text == nil && v.B64 == nil):
		return nil, fmt.Errorf("%w: value needs text or b64", dbstore.ErrInvalidInput)
	case v.Text != nil && v.B64 != nil:
		return nil, fmt.Errorf("%w: value gives both text and b64", dbstore.ErrInvalidInput)
	case v.Text != nil:
		return []byte(*v.Text), nil
	}
	b, err := base64.StdEncoding.DecodeString(*v.B64)
	if err != nil {
		return nil, fmt.Errorf("%w: b64 value: %v", dbstore.ErrInvalidInput, err)
	}
	return b, nil
}

// Encode renders bytes as text when valid UTF-8, else base64.
func Encode(b []byte) Value {
	if utf8.Valid(b) {
		s := string(b)
		return Value{Text: &s}
	}
	s := base64.StdEncoding.EncodeToString(b)
	return Value{B64: &s}
}

// ObjectInfo describes one object's metadata.
type ObjectInfo struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Size        int64             `json:"size"`
	SHA256      string            `json:"sha256"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Revision    int64             `json:"revision"`
	UpdatedAt   string            `json:"updated_at"`
}

// BucketInfo describes one object bucket.
type BucketInfo struct {
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	ObjectCount int64  `json:"object_count,omitempty"`
	TotalBytes  int64  `json:"total_bytes,omitempty"`
}

// Input is the typed input for object operations.
type Input struct {
	Bucket      string            `json:"bucket,omitempty"`
	Key         string            `json:"key,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Value       *Value            `json:"value,omitempty"`
	In          string            `json:"in,omitempty"`
	Out         string            `json:"out,omitempty"`
	Prefix      string            `json:"prefix,omitempty"`
	Start       string            `json:"start,omitempty"`
	Limit       int               `json:"limit,omitempty"`
	MaxBytes    int               `json:"max_bytes,omitempty"`
	Since       string            `json:"since,omitempty"`
	IfRevision  string            `json:"if_revision,omitempty"`
	IfAbsent    bool              `json:"if_absent,omitempty"`
	OnConflict  string            `json:"on_conflict,omitempty"`
	Replace     bool              `json:"replace,omitempty"`
	Remote      string            `json:"remote,omitempty"`
	Destination string            `json:"destination,omitempty"`
	Checksum    string            `json:"checksum,omitempty"`
	Delete      bool              `json:"delete,omitempty"`
}

func decodeInput(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) == 0 {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: obj input: %v", dbstore.ErrInvalidInput, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return in, fmt.Errorf("%w: trailing obj input", dbstore.ErrInvalidInput)
	}
	return in, nil
}

// DecodeInput decodes raw JSON into Input rejecting unknown fields.
func DecodeInput(raw json.RawMessage) (Input, error) {
	return decodeInput(raw)
}

func (in Input) bounds() (limit, maxBytes int, err error) {
	limit, maxBytes = in.Limit, in.MaxBytes
	if limit == 0 {
		limit = DefaultLimit
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if limit < 0 || limit > MaxLimit || maxBytes < 0 || maxBytes > MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, MaxLimit, MaxMaxBytes)
	}
	return limit, maxBytes, nil
}

func checkBucketName(name string) error {
	if name == "" || len(name) > MaxBucketName || !utf8.ValidString(name) {
		return fmt.Errorf("%w: bucket name must be 1..%d bytes of valid UTF-8", dbstore.ErrInvalidInput, MaxBucketName)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: bucket name cannot contain slashes", dbstore.ErrInvalidInput)
	}
	return nil
}

func checkObjectKey(key string) error {
	if key == "" || len(key) > MaxKeyLength || !utf8.ValidString(key) {
		return fmt.Errorf("%w: object key must be 1..%d bytes of valid UTF-8", dbstore.ErrInvalidInput, MaxKeyLength)
	}
	return nil
}

type cursorBinding struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Query    string `json:"query"`
}

type cursor struct {
	SchemaVersion string        `json:"schema_version"`
	Kind          string        `json:"kind"`
	Token         string        `json:"token"`
	Binding       cursorBinding `json:"binding"`
}

type page struct {
	Returned  int     `json:"returned"`
	Limit     int     `json:"limit"`
	Truncated bool    `json:"truncated"`
	Bytes     int     `json:"bytes"`
	Cursor    *cursor `json:"cursor,omitempty"`
}

func newPage(target, query, last string, returned, limit, bytes int, truncated bool) page {
	p := page{Returned: returned, Limit: limit, Truncated: truncated, Bytes: bytes}
	if truncated {
		p.Cursor = &cursor{
			SchemaVersion: "bashy-cursor-v1",
			Kind:          "pagination",
			Token:         "k:" + hex.EncodeToString([]byte(last)),
			Binding:       cursorBinding{Provider: "db", Target: target, Query: query},
		}
	}
	return p
}

func decodeCursor(since string) (string, bool, error) {
	if since == "" {
		return "", false, nil
	}
	var c cursor
	if err := json.Unmarshal([]byte(since), &c); err != nil {
		if strings.HasPrefix(since, "k:") {
			raw, err := hex.DecodeString(since[2:])
			if err != nil {
				return "", false, fmt.Errorf("%w: raw cursor token: %v", dbstore.ErrInvalidInput, err)
			}
			return string(raw), true, nil
		}
		return "", false, fmt.Errorf("%w: parse cursor: %v", dbstore.ErrInvalidInput, err)
	}
	if c.SchemaVersion != "bashy-cursor-v1" || c.Kind != "pagination" {
		return "", false, fmt.Errorf("%w: cursor schema version %q kind %q", dbstore.ErrInvalidInput, c.SchemaVersion, c.Kind)
	}
	hexKey, ok := strings.CutPrefix(c.Token, "k:")
	if !ok {
		return "", false, fmt.Errorf("%w: cursor token prefix", dbstore.ErrInvalidInput)
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return "", false, fmt.Errorf("%w: cursor token hex: %v", dbstore.ErrInvalidInput, err)
	}
	return string(key), true, nil
}

// Spec is the operation declaration shared by CLI, --explain, MCP and owner IPC.
func Spec(name string, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
	s := dblifecycle.Operation{
		Name:          name,
		MutationClass: "read",
		Hold:          "none",
		Atomic:        "op",
		DryRun:        "local-preview",
		Modes:         []string{"memory", "persistent"},
	}
	fs := func(tokens ...string) []string {
		if mode == dbstore.Persistent {
			return tokens
		}
		var out []string
		for _, t := range tokens {
			if !strings.HasPrefix(t, "fs.") {
				out = append(out, t)
			}
		}
		return out
	}
	switch a {
	case dblifecycle.AccessRead:
		s.Effects = fs("fs.read", "db.read")
	case dblifecycle.AccessWrite:
		s.MutationClass, s.Hold = "write", "shared-writer"
		s.Effects = fs("fs.write", "db.write")
	case dblifecycle.AccessExclusive:
		s.MutationClass, s.Hold = "destructive", "exclusive"
		s.Effects = fs("fs.write", "db.write", "db.delete")
	}
	switch name {
	case OpExport:
		s.Effects = append(fs("fs.read", "db.read"), "fs.write")
		s.MutationClass, s.Hold = "read", "none"
	case OpImport:
		s.Effects = append([]string{"fs.read"}, s.Effects...)
	case OpTransfer:
		s.Effects = append(s.Effects, "net.egress")
	}
	return s
}
