package weave

import "testing"

// An identity-only paired host's agent shell has no $CLOUDBOX_TOKEN (it cannot
// render `secret env`); the session token then comes from the host's
// secrets.map binding, and the environment still wins when it is set.
func TestSessionTokenResolvesThroughTheSecretsBinding(t *testing.T) {
	prev := boundSessionToken
	t.Cleanup(func() { boundSessionToken = prev })
	var asked string
	boundSessionToken = func(name string) (string, bool) {
		asked = name
		return "bound-session-token", true
	}

	t.Setenv("CLOUDBOX_TOKEN", "")
	_, token, err := resolveSessionCredentials(nil)
	if err != nil || token != "bound-session-token" || asked != "CLOUDBOX_TOKEN" {
		t.Fatalf("unset env: token=%q err=%v asked=%q", token, err, asked)
	}

	asked = ""
	_, token, err = resolveSessionCredentials(&SessionPointer{TokenRef: "MY_SESSION_TOKEN"})
	if err != nil || token != "bound-session-token" || asked != "MY_SESSION_TOKEN" {
		t.Fatalf("pointer ref: token=%q err=%v asked=%q", token, err, asked)
	}

	asked = ""
	t.Setenv("CLOUDBOX_TOKEN", "env-token")
	_, token, err = resolveSessionCredentials(nil)
	if err != nil || token != "env-token" || asked != "" {
		t.Fatalf("env set: token=%q err=%v asked=%q (the binding must not be consulted)", token, err, asked)
	}
}
