package gws

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

func rawItems(n int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		out[i] = json.RawMessage(`{"id":"` + itoa(i) + `"}`)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func TestPaginateFirstAndResume(t *testing.T) {
	b := Binding{Target: "drive:files", Query: "q=x", Revision: "r1"}
	recs := rawItems(120)
	p, err := Paginate(recs, 0, 50, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 50 || p.Complete || p.NextToken == "" {
		t.Fatalf("first page = %d items complete=%v token=%q", len(p.Items), p.Complete, p.NextToken)
	}
	off, err := ResumeOffset(p.NextToken, b)
	if err != nil {
		t.Fatal(err)
	}
	if off != 50 {
		t.Errorf("resume offset = %d, want 50", off)
	}
	p2, err := Paginate(recs, off, 50, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 50 || p2.Complete {
		t.Fatalf("second page = %d items complete=%v", len(p2.Items), p2.Complete)
	}
	off2, err := ResumeOffset(p2.NextToken, b)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := Paginate(recs, off2, 50, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Items) != 20 || !p3.Complete || p3.NextToken != "" {
		t.Fatalf("last page = %d items complete=%v token=%q", len(p3.Items), p3.Complete, p3.NextToken)
	}
}

// A token is bound: reuse under another query fails typed, never replays.
func TestTokenCrossBindingRejected(t *testing.T) {
	b1 := Binding{Target: "drive:files", Query: "q=x"}
	b2 := Binding{Target: "drive:files", Query: "q=y"}
	p, err := Paginate(rawItems(120), 0, 50, b1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResumeOffset(p.NextToken, b2); err == nil {
		t.Fatal("cross-binding token reuse must fail")
	} else if err.Code != CodeCursorMismatch {
		t.Fatalf("code = %q, want cursor-mismatch", err.Code)
	}
	if err := b2.Validate(p.NextToken, b1); err == nil {
		t.Fatal("Validate must reject cross-binding reuse")
	}
}

// Foreign strings and out-of-range offsets fail typed.
func TestForeignAndRangedTokens(t *testing.T) {
	b := Binding{Target: "gmail:messages", Query: "in:inbox"}
	for _, tok := range []string{"12345", "since:yesterday", "gws1.abc.deadbeef", "other.1.abcdef0123456789"} {
		if _, err := ResumeOffset(tok, b); err == nil {
			t.Errorf("token %q must fail", tok)
		} else if err.Code != CodeCursorMismatch {
			t.Errorf("token %q code = %q, want cursor-mismatch", tok, err.Code)
		}
	}
	if _, err := Paginate(rawItems(10), 11, 50, b); err == nil {
		t.Error("out-of-range offset must fail")
	} else if err.Code != CodeCursorMismatch {
		t.Errorf("code = %q, want cursor-mismatch", err.Code)
	}
}

func TestPaginateBounds(t *testing.T) {
	b := Binding{Target: "drive:files"}
	if _, err := Paginate(rawItems(10), 0, MaxPageSize+1, b); err == nil {
		t.Error("oversize page must fail")
	} else if err.Code != CodeOutputLimit {
		t.Errorf("code = %q, want output-limit", err.Code)
	}
	big := make([]json.RawMessage, MaxTotal+1)
	for i := range big {
		big[i] = json.RawMessage(`{}`)
	}
	if _, err := Paginate(big, 0, 50, b); err == nil {
		t.Error("over-total listing must fail")
	} else if err.Code != CodeOutputLimit {
		t.Errorf("code = %q, want output-limit", err.Code)
	}
}

// Pages emit the ONE discriminator: schema_version, loom-v2 native.
func TestEmitPageEnvelope(t *testing.T) {
	b := Binding{Target: "tasks:lists"}
	p, err := Paginate(rawItems(3), 0, 50, b)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := EmitPage(&buf, "gws tasks list", p); code != weavecli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != weavecli.SchemaVersion {
		t.Errorf("schema_version = %v, want %q", env["schema_version"], weavecli.SchemaVersion)
	}
	if env["status"] != "ok" {
		t.Errorf("status = %v, want ok (complete walk)", env["status"])
	}
	if items, ok := env["items"].([]any); !ok || len(items) != 3 {
		t.Errorf("items = %v, want 3 records", env["items"])
	}
}

func TestEmitPagePartial(t *testing.T) {
	b := Binding{Target: "drive:files"}
	p, err := Paginate(rawItems(120), 0, 50, b)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	EmitPage(&buf, "gws drive list", p)
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["status"] != "partial" {
		t.Errorf("status = %v, want partial (incomplete walk)", env["status"])
	}
	if env["next_page_token"] == "" {
		t.Error("partial page must carry the resume token")
	}
}

func TestEmitNDJSON(t *testing.T) {
	var buf bytes.Buffer
	if code := EmitNDJSON(&buf, "gws gmail list", rawItems(2), true); code != weavecli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 2 records + 1 completion marker", len(lines))
	}
	for i, ln := range lines {
		var env map[string]any
		if err := json.Unmarshal([]byte(ln), &env); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if env["schema_version"] != weavecli.SchemaVersion {
			t.Errorf("line %d schema_version = %v, want %q", i, env["schema_version"], weavecli.SchemaVersion)
		}
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &marker); err != nil {
		t.Fatal(err)
	}
	if marker["complete"] != true {
		t.Errorf("final line must be the completion marker, got %v", lines[2])
	}
}
