package tessaro

import "testing"

func TestSubVerbMapping(t *testing.T) {
	cases := map[string]string{
		"login": "register", "signin": "register", "pair": "register",
		"logout": "unpair", "signout": "unpair", "unpair": "unpair",
		"status": "status", "whoami": "status",
	}
	for verb, want := range cases {
		if got := subVerbs[verb]; got != want {
			t.Errorf("subVerbs[%q] = %q, want %q", verb, got, want)
		}
	}
	// `open` is handled specially (no agent needed), not via subVerbs.
	if _, ok := subVerbs["open"]; ok {
		t.Error("open must not be in subVerbs (handled without the agent)")
	}
}

func TestCommandsBuild(t *testing.T) {
	if NewTessaroCmd().Name() != "tessaro" {
		t.Error("tessaro command name wrong")
	}
	if NewLoginCmd().Name() != "login" {
		t.Error("login command name wrong")
	}
}
