package aws

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func TestExplicitProfileKeys(t *testing.T) {
	t.Setenv(EnvAccessKeyID, "")
	t.Setenv(EnvSecretAccessKey, "")
	path := filepath.Join(t.TempDir(), "credentials")
	t.Setenv(EnvSharedCredentials, path)
	contents := "[owl]\naws_access_key_id=owl\naws_secret_access_key=" + accountstest.CanarySecret + "\n[otter]\naws_access_key_id=otter\naws_secret_access_key=" + accountstest.CanarySecret + "\n"
	if e := os.WriteFile(path, []byte(contents), 0600); e != nil {
		t.Fatal(e)
	}
	profiles, err := ConfiguredProfiles()
	if err != nil || len(profiles) != 2 || profiles[0] != "otter" || profiles[1] != "owl" {
		t.Fatal("profile discovery failed")
	}
	accountstest.AssertNoSecret(t, profiles, err)
	for _, p := range []string{"owl", "otter"} {
		keys, e := ResolveProfileKeys(p)
		if e != nil {
			t.Fatal(e)
		}
		if keys.AccessKeyID != p || keys.SecretAccessKey != accountstest.CanarySecret {
			t.Fatal("profile crossover")
		}
		accountstest.AssertNoSecret(t, e, keys.AccessKeyID)
	}
	_, e := ResolveProfileKeys("missing")
	if e == nil {
		t.Fatal("missing profile fell back")
	}
	accountstest.AssertNoSecret(t, e)
	t.Setenv(EnvProfile, "owl")
	t.Setenv(EnvAccessKeyID, "owl")
	t.Setenv(EnvSecretAccessKey, accountstest.CanarySecret)
	_, e = ResolveProfileKeys("owl")
	if e == nil {
		t.Fatal("ambiguous credential sources accepted")
	}
	accountstest.AssertNoSecret(t, e)
	t.Setenv(EnvSharedCredentials, filepath.Join(t.TempDir(), "missing"))
	_, e = ResolveProfileKeys("otter")
	if e == nil {
		t.Fatal("environment profile crossover")
	}
	accountstest.AssertNoSecret(t, e)
	keys, e := ResolveProfileKeys("owl")
	if e != nil || keys.SecretAccessKey != accountstest.CanarySecret {
		t.Fatal("explicit environment profile refused")
	}
	accountstest.AssertNoSecret(t, e)
}
