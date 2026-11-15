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

// TestParseGenie: the genie placement key is shaped config the protected
// button reads — a corner (top-right by default), an optional anchor, and
// hidden; a position that is not a corner fails loudly.
func TestParseGenie(t *testing.T) {
	m, err := Parse([]byte(`{"genie": {"hidden": true}}`))
	if err != nil || m.Genie == nil || !m.Genie.Hidden || m.Genie.Position != GenieDefaultPosition {
		t.Fatalf("genie hidden = %+v, %v", m.Genie, err)
	}
	m, err = Parse([]byte(`{"genie": {"position": "bottom-left", "anchor": "#toolbar, header"}, "properties": ["title"]}`))
	if err != nil || m.Genie.Position != "bottom-left" || m.Genie.Anchor != "#toolbar, header" || len(m.Properties) != 1 {
		t.Fatalf("genie placed = %+v, %v", m.Genie, err)
	}
	m, err = Parse([]byte(`{"genie": {"examples": [" make the ship faster ", "add a high score table"]}}`))
	if err != nil || len(m.Genie.Examples) != 2 || m.Genie.Examples[0] != "make the ship faster" {
		t.Fatalf("genie examples = %+v, %v", m.Genie, err)
	}
	for _, bad := range []string{
		`{"genie": {"position": "middle"}}`,
		`{"genie": {"anchor": "<script>"}}`,
		`{"genie": {"shortcut": "g"}}`,
		`{"genie": {"examples": [""]}}`,
		`{"genie": {"examples": ["<b>bold</b>"]}}`,
		`{"genie": {"examples": ["a", "b", "c", "d", "e", "f", "g"]}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) accepted", bad)
		}
	}
}

// TestParseGenieVoice (Sprint 624 V2): the mic is on unless the app turns it
// off, and no manifest at all means on — the key is a way to opt out of a
// wish-box input, not to opt into one.
func TestParseGenieVoice(t *testing.T) {
	var none *Genie
	if !none.VoiceOn() {
		t.Error("an app with no genie placement must still offer the mic")
	}
	m, err := Parse([]byte(`{"genie": {"position": "top-left"}}`))
	if err != nil || !m.Genie.VoiceOn() {
		t.Fatalf("a placement without the voice key = %+v, %v; want voice on", m.Genie, err)
	}
	m, err = Parse([]byte(`{"genie": {"voice": false}}`))
	if err != nil || m.Genie.VoiceOn() {
		t.Fatalf("voice false = %+v, %v; want voice off", m.Genie, err)
	}
	m, err = Parse([]byte(`{"genie": {"voice": true}}`))
	if err != nil || !m.Genie.VoiceOn() {
		t.Fatalf("voice true = %+v, %v; want voice on", m.Genie, err)
	}
	if _, err := Parse([]byte(`{"genie": {"voice": "yes"}}`)); err == nil {
		t.Error(`Parse({"genie": {"voice": "yes"}}) accepted; the key is a boolean`)
	}
}
