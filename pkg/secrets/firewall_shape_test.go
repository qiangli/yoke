package secrets

import (
	"slices"
	"testing"
)

// THE HOLE IN THE MIDDLE OF THE FIREWALL.
//
// Scrubbing used to be by vault NAME alone — "a variable the vault does not project is
// never touched". So a credential the operator held but had not mapped was treated as
// not a credential, and went to every spawned agent.
//
// DHNT_API_KEY was exactly that: live in the environment, absent from the map, and
// therefore handed to every agent. It is the pooled-LLM gateway key, so an agent whose
// OWN key had been correctly scrubbed could still spend through it.
func TestUnmappedCredentialsAreStillScrubbed(t *testing.T) {
	t.Setenv(AllowAgentSecretsEnv, "")

	env := []string{
		"DHNT_API_KEY=sk-pooled-gateway", // the real one: a credential the vault never mapped
		"SOME_VENDOR_TOKEN=t",
		"CUSTOM_SECRET=s",
		"PATH=/usr/bin",
		"HOME=/home/x",
	}
	got := ScrubAgentEnv(env)

	for _, leaked := range []string{"DHNT_API_KEY=sk-pooled-gateway", "SOME_VENDOR_TOKEN=t", "CUSTOM_SECRET=s"} {
		if slices.Contains(got, leaked) {
			t.Errorf("a credential the vault does not project survived the scrub: %s", leaked)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/x"} {
		if !slices.Contains(got, keep) {
			t.Errorf("the scrub removed an ordinary variable: %s", keep)
		}
	}
}

// IT USED TO FAIL OPEN.
//
//	names := VaultEnvNames()
//	if len(names) == 0 { return env }   // no vault -> no scrub -> the whole keyring
//
// A security boundary that disables itself when it cannot read its own config is not a
// boundary. With no vault at all, the shape rule must still hold — an unreadable vault
// makes the firewall STRICTER, never absent.
func TestNoVaultMeansStricterNotAbsent(t *testing.T) {
	t.Setenv(AllowAgentSecretsEnv, "")
	// No vault map is consulted here; whatever VaultEnvNames() returns, the shape rule
	// alone must catch a credential-shaped name.
	got := ScrubAgentEnv([]string{"ANTHROPIC_API_KEY=sk-live", "PATH=/bin"})

	if slices.Contains(got, "ANTHROPIC_API_KEY=sk-live") {
		t.Fatal("with no vault names, the firewall handed over a credential — it failed OPEN")
	}
	if !slices.Contains(got, "PATH=/bin") {
		t.Error("PATH was scrubbed")
	}
}

// Names, never values. A value-based rule would have to inspect the operator's secrets
// to decide whether to hide them, and would leak through anything that did not look
// secret enough.
func TestLooksLikeCredentialIsShapeBased(t *testing.T) {
	credential := []string{
		"ANTHROPIC_API_KEY", "ZAI_API_KEY", "DHNT_API_KEY", "GITHUB_TOKEN",
		"OPENAI_APIKEY", "FOO_SECRET", "DB_PASSWORD", "AWS_SECRET_ACCESS_KEY",
		"GH_TOKEN", "NPM_TOKEN", "SERVICE_PRIVATE_KEY", "X_SESSION_TOKEN",
	}
	for _, n := range credential {
		if !looksLikeCredential(n) {
			t.Errorf("%s is credential-shaped and was not recognised", n)
		}
	}

	ordinary := []string{"PATH", "HOME", "TERM", "LANG", "EDITOR", "GOPATH", "SHELL", "KEYBOARD_LAYOUT"}
	for _, n := range ordinary {
		if looksLikeCredential(n) {
			t.Errorf("%s is an ordinary variable and was treated as a credential", n)
		}
	}
}

// The opt-out still works — an operator who explicitly wants the keyring passed through
// gets it. Explicit is fine; silent is not.
func TestOperatorCanStillOptOut(t *testing.T) {
	t.Setenv(AllowAgentSecretsEnv, "1")
	got := ScrubAgentEnv([]string{"ANTHROPIC_API_KEY=sk-live"})
	if !slices.Contains(got, "ANTHROPIC_API_KEY=sk-live") {
		t.Error("the explicit opt-out did not pass the credential through")
	}
}
