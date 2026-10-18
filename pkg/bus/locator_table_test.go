package bus

import "testing"

func TestLocatorTable(t *testing.T) {
	pinLocalHost(t, "here")
	cases := []struct {
		name, body, from string
		ref              string
		cross            bool
		host             string
	}{
		{"local sender unscoped", "see dir:pkg/bus", "carol", "pkg/bus", false, ""},
		{"same host sender", "see dir:pkg/bus", "carol@here", "pkg/bus", false, "here"},
		{"remote sender pinned", "see dir:pkg/bus", "carol@far", "dir:pkg/bus@far", true, "far"},
		{"explicit remote host", "see dir:pkg/bus@other", "carol", "dir:pkg/bus@other", true, "other"},
		{"remote portable scheme", "see sprint:446", "carol@far", "sprint:446", false, ""},
	}
	for _, c := range cases {
		got := MessageLocators(c.body, c.from)
		if len(got) != 1 {
			t.Errorf("%s: got %+v", c.name, got)
			continue
		}
		g := got[0]
		if g.Ref != c.ref || g.CrossHost != c.cross || g.Host != c.host {
			t.Errorf("%s: got %+v, want ref=%q cross=%v host=%q", c.name, g, c.ref, c.cross, c.host)
		}
		if c.cross && g.Note == "" {
			t.Errorf("%s: cross-host ref lacks note", c.name)
		}
	}
}

func TestLocatorPathHint(t *testing.T) {
	for body, want := range map[string]bool{
		"look at /Users/x/notes.md": true,
		"see dir:/tmp/x":            false,
		"https://example.com/a/b":   false,
		"plain words and/or either": false,
	} {
		if got := remotePathHint(body) != ""; got != want {
			t.Errorf("remotePathHint(%q) = %v, want %v", body, got, want)
		}
	}
}
