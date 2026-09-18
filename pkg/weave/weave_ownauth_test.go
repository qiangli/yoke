// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/secrets"
)

func TestNamedYcodeChildEnvPreservesResolvedCredentialNames(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	root := t.TempDir()
	cat := fleet.New(fleet.WithRoot(root))
	if err := cat.SaveAgent(fleet.Agent{Name: "disposable-ycode", Tool: "ycode", Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	previous := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { fleetCatalog = previous })

	launch, err := weaveResolveAgent("disposable-ycode")
	if err != nil {
		t.Fatal(err)
	}
	if launch == nil {
		t.Fatal("named ycode launch did not resolve")
	}
	ambient := []string{"PATH=/usr/bin", "ZAI_API_KEY=selected-model-credential", "OPENAI_API_KEY=unrelated"}
	it := &weaveItem{ID: 48, Owner: "disposable-ycode-a"}
	child := weaveChildEnv(ambient, "/ws/issue-48", "agent/weave-issue-48", "main", t.TempDir(), it, launch)
	if !envHas(child, "ZAI_API_KEY") || envHas(child, "OPENAI_API_KEY") {
		t.Errorf("resolved launch environment parity failed: names=%v", names(child))
	}
}

// The live launch assertion: the selected model credential survives, unrelated
// operator credentials do not, and workspace containment remains intact.
// Failures render names only so no ambient value can enter test output.
func TestWeaveChildEnvPreservesOnlyResolvedCredentialNames(t *testing.T) {
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")

	ambient := []string{
		"PATH=/usr/bin",
		"HOME=/home/operator",
		"PWD=/origin/repo",
		"OLDPWD=/origin/elsewhere",
		"DEEPSEEK_API_KEY=selected-model-credential",
		"OPENAI_API_KEY=unrelated-operator-credential",
		"AWS_SECRET_ACCESS_KEY=unrelated-cloud-credential",
	}
	launch := &weaveAgentLaunch{PreserveEnv: []string{"DEEPSEEK_API_KEY"}}
	it := &weaveItem{ID: 105, Title: "t", Body: "b", Owner: "007-a"}
	got := weaveChildEnv(ambient, "/ws/issue-105", "agent/weave-issue-105", "main", t.TempDir(), it, launch)

	if !envHas(got, "DEEPSEEK_API_KEY") {
		t.Errorf("child env lost selected model credential: names=%v", names(got))
	}
	for _, banned := range []string{"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if envHas(got, banned) {
			t.Errorf("child env carries unrelated operator credential %s", banned)
		}
	}
	if !slices.Contains(got, "PWD=/ws/issue-105") || envHas(got, "OLDPWD") {
		t.Errorf("workspace containment failed: names=%v", names(got))
	}
	if !envHas(got, "WEAVE_ISSUE") || !envHas(got, "WEAVE_AGENT") {
		t.Errorf("weave stamps missing: names=%v", names(got))
	}
}

// A raw/non-agent launch has no resolved credential contract. It must remain
// deny-by-default and must not panic or restore any credential-shaped name.
func TestWeaveChildEnvNilLaunchPreservesNoCredentials(t *testing.T) {
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	ambient := []string{"PATH=/usr/bin", "DEEPSEEK_API_KEY=operator-credential"}
	it := &weaveItem{ID: 106, Owner: "raw-a"}
	got := weaveChildEnv(ambient, "/ws/issue-106", "agent/weave-issue-106", "main", t.TempDir(), it, nil)
	if envHas(got, "DEEPSEEK_API_KEY") {
		t.Errorf("nil launch reopened a credential: names=%v", names(got))
	}
}

// A declared name absent from the parent is not manufactured.
func TestWeaveChildEnvAbsentCredentialIsNoOp(t *testing.T) {
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	launch := &weaveAgentLaunch{PreserveEnv: []string{"DEEPSEEK_API_KEY"}}
	it := &weaveItem{ID: 107, Owner: "007-a"}
	got := weaveChildEnv([]string{"PATH=/usr/bin"}, "/ws/issue-107", "agent/weave-issue-107", "main", t.TempDir(), it, launch)
	if envHas(got, "DEEPSEEK_API_KEY") {
		t.Errorf("absent credential was manufactured: names=%v", names(got))
	}
}

// Duplicate metadata or parent entries cannot create an ambiguous child env.
func TestWeaveChildEnvDoesNotDuplicateCredential(t *testing.T) {
	t.Setenv(secrets.AllowAgentSecretsEnv, "0")
	launch := &weaveAgentLaunch{PreserveEnv: []string{"DEEPSEEK_API_KEY", "DEEPSEEK_API_KEY"}}
	it := &weaveItem{ID: 108, Owner: "007-a"}
	ambient := []string{"PATH=/usr/bin", "DEEPSEEK_API_KEY=first", "DEEPSEEK_API_KEY=second"}
	got := weaveChildEnv(ambient, "/ws/issue-108", "agent/weave-issue-108", "main", t.TempDir(), it, launch)
	count := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "DEEPSEEK_API_KEY=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("credential appears %d times: names=%v", count, names(got))
	}
}

func envHas(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

// names renders an env as names only. Diagnostics must never print values.
func names(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out = append(out, kv[:i])
		}
	}
	return out
}
