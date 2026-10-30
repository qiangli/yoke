// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package appserver

import (
	"reflect"
	"testing"
)

func TestParseAndArgv(t *testing.T) {
	m, err := Parse([]byte(`{"server": {"args": ["serve", "-addr", "$ADDR", "-port=$PORT", "-data", "$DATA"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Server.Main != "." {
		t.Fatalf("main = %q, want the default .", m.Server.Main)
	}
	got := m.Server.Argv("127.0.0.1:4100", 4100, "/d")
	want := []string{"serve", "-addr", "127.0.0.1:4100", "-port=4100", "-data", "/d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if m, err := Parse([]byte(`{}`)); err != nil || m.Server != nil {
		t.Fatalf("serverless manifest = %+v, %v", m, err)
	}
	if m, err := Parse([]byte(`{"server": {"main": "./cmd/srv/", "args": ["$PORT"]}}`)); err != nil || m.Server.Main != "cmd/srv" {
		t.Fatalf("main cleaned = %+v, %v", m.Server, err)
	}
	for _, bad := range []string{
		`{"server": {"args": []}}`,
		`{"server": {"main": "/abs", "args": ["$ADDR"]}}`,
		`{"server": {"main": "a/../../x", "args": ["$ADDR"]}}`,
		`{"server": {"main": "a\\b", "args": ["$ADDR"]}}`,
		`{"server": {"args": ["$ADDR"], "env": {}}}`,
		`{"server": {"args": ["$ADDR"]}} {}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) accepted", bad)
		}
	}
}
