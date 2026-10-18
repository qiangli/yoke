package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

func rawItems(vals ...int) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(vals))
	for _, v := range vals {
		out = append(out, json.RawMessage(fmt.Sprintf(`{"id":%d}`, v)))
	}
	return out
}

var testBinding = Binding{Target: "ec2:describe-instances", Query: "state=running", Revision: "v4"}

func TestPaginateFirstAndResume(t *testing.T) {
	records := rawItems(1, 2, 3)
	p, err := Paginate(records, 0, 2, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	if p.Complete || p.NextToken == "" || len(p.Items) != 2 || p.Total != 3 {
		t.Fatalf("page = %+v, want partial 2/3 with token", p)
	}
	off, err := ResumeOffset(p.NextToken, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	if off != 2 {
		t.Fatalf("offset = %d, want 2", off)
	}
	p2, err := Paginate(records, off, 2, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Complete || p2.NextToken != "" || len(p2.Items) != 1 {
		t.Fatalf("page2 = %+v, want complete 1/3", p2)
	}
}

func TestTokenCrossBindingRejected(t *testing.T) {
	records := rawItems(1, 2, 3)
	p, err := Paginate(records, 0, 2, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	other := Binding{Target: "iam:list-users", Query: "state=running", Revision: "v4"}
	if _, err := ResumeOffset(p.NextToken, other); err == nil {
		t.Fatal("cross-binding token reuse must fail")
	} else if err.Code != CodeCursorMismatch {
		t.Fatalf("code = %q, want cursor-mismatch", err.Code)
	}
}

func TestForeignAndRangedTokens(t *testing.T) {
	for _, tok := range []string{"bogus", "gws1.0.deadbeef", "aws1.-1.abcdef0123456789", "aws1.x.abcdef0123456789"} {
		if _, err := ResumeOffset(tok, testBinding); err == nil {
			t.Errorf("token %q must fail", tok)
		} else if err.Code != CodeCursorMismatch {
			t.Errorf("token %q code = %q, want cursor-mismatch", tok, err.Code)
		}
	}
	if _, err := Paginate(rawItems(1), 5, 10, testBinding); err == nil {
		t.Error("out-of-range offset must fail")
	}
	if _, err := Paginate(rawItems(1), 0, MaxPageSize+1, testBinding); err == nil {
		t.Error("over-max page size must fail")
	} else if err.Code != CodeOutputLimit {
		t.Errorf("code = %q, want output-limit", err.Code)
	}
	big := make([]json.RawMessage, MaxTotal+1)
	if _, err := Paginate(big, 0, 10, testBinding); err == nil {
		t.Error("over-max total must fail")
	}
}

func TestEmitPageEnvelope(t *testing.T) {
	p, err := Paginate(rawItems(1, 2), 0, 50, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := EmitPage(&buf, "aws ec2 describe-instances", p); code != weavecli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var env struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Total         int    `json:"total"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.SchemaVersion != weavecli.SchemaVersion || env.Status != "ok" || env.Total != 2 {
		t.Errorf("envelope = %+v, want loom-v2 ok total=2", env)
	}
}

func TestEmitNDJSONEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if code := EmitNDJSON(&buf, "aws ec2 describe-instances", rawItems(1, 2), true); code != weavecli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 2 items + marker", len(lines))
	}
	for _, ln := range lines {
		var env map[string]any
		if err := json.Unmarshal([]byte(ln), &env); err != nil {
			t.Fatal(err)
		}
		if env["schema_version"] != weavecli.SchemaVersion {
			t.Errorf("line missing loom-v2 discriminator: %q", ln)
		}
	}
}
