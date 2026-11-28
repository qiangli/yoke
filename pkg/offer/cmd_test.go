package offer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

func TestOfferRemoteCallPeerUsesTypedDefinition(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bpeer_test" {
			t.Errorf("missing peer credential")
		}
		if r.URL.Path != "/offers/sample" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]any{"definition": ToolDefinition{Name: "sample", Class: "tool", Input: map[string]any{"type": "object", "required": []string{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer"}}, "additionalProperties": false}, Unit: "call"}, "digest": "digest"})
			return
		}
		calls++
		var body struct {
			Args map[string]any `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Args["count"] != float64(3) {
			t.Errorf("args %#v", body.Args)
		}
		_, _ = w.Write([]byte(`{"result":{"ok":true},"usage":{"calls":1}}`))
	}))
	defer srv.Close()
	t.Setenv("BASHY_HOME", t.TempDir())
	if err := toolcmd.SavePeer("host", toolcmd.PeerDoor{URL: srv.URL, Credential: "bpeer_test"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{{[]string{"host", "sample", "--count", "3", "--json"}, false}, {[]string{"host", "sample", "--other", "3"}, true}} {
		cmd := newRemoteCallCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(tc.args)
		err := cmd.Execute()
		if (err != nil) != tc.wantErr {
			t.Fatalf("%v error %v: %s", tc.args, err, out.String())
		}
		if !tc.wantErr && !strings.Contains(out.String(), `"ok":true`) {
			t.Fatalf("output %s", out.String())
		}
	}
	if calls != 1 {
		t.Fatalf("POST count %d", calls)
	}
}

func TestOfferFilesRegistersGatedApp(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	old := outpostRun
	var calls [][]string
	outpostRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch strings.Join(args[:min(len(args), 2)], " ") {
		case "apps list":
			return []byte(`{"apps":[]}`), nil
		case "apps secret":
			return []byte("test-secret\n"), nil
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { outpostRun = old })
	if err := RegisterFilesShare(t.Context(), "docs", []string{"team"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	dir, err := broker.OffersDir()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := broker.LoadOffer(dir, "files-docs")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "files-docs" || rec.SSOSecret != "test-secret" || len(rec.Audience.Circles) != 1 {
		t.Fatalf("record %+v", rec)
	}
	var sawAdd, sawShare bool
	for _, c := range calls {
		line := strings.Join(c, " ")
		if strings.Contains(line, "apps add offer-files-docs") && strings.Contains(line, "--require-login --trust-cloud-identity") {
			sawAdd = true
		}
		if strings.Contains(line, "apps share offer-files-docs --circle team") {
			sawShare = true
		}
	}
	if !sawAdd || !sawShare {
		t.Fatalf("outpost calls %v", calls)
	}
}
