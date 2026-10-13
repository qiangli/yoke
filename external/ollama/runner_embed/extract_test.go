package runner_embed

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const testVersion = "bashy-ollama-runner-test"
const testIdentity = "https://github.com/qiangli/bashy/.github/workflows/ollama-runner.yml@refs/tags/" + testVersion

func fakeEmbed(t *testing.T, data []byte) {
	t.Helper()
	oldCompressed, oldBundle, oldRelease, oldVerifier := compressedRunner, embeddedRunnerBundle, runnerRelease, verifyRunner
	t.Cleanup(func() {
		compressedRunner, embeddedRunnerBundle, runnerRelease, verifyRunner = oldCompressed, oldBundle, oldRelease, oldVerifier
	})
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	compressedRunner = buf.Bytes()
	embeddedRunnerBundle = []byte(`{"signed":"fixture"}`)
	runnerRelease = releaseMetadata{testVersion, testIdentity, sha256Hex(data)}
	verifyRunner = func(_ context.Context, path string, bundle []byte, pin string, signer binmgr.Sigstore) error {
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if sha256Hex(got) != pin {
			return fmt.Errorf("wrong executable SHA")
		}
		if signer.Identity != testIdentity || signer.Issuer != binmgr.GitHubActionsIssuer {
			return fmt.Errorf("wrong signing identity")
		}
		if !bytes.Equal(bundle, embeddedRunnerBundle) {
			return fmt.Errorf("wrong bundle")
		}
		return nil
	}
}

func TestEnsureRunnerMissingReleaseFailsClosed(t *testing.T) {
	fakeEmbed(t, []byte("runner"))
	runnerRelease.sha256 = ""
	if Available() {
		t.Fatal("available without pin")
	}
	if _, err := EnsureRunner(t.TempDir()); err == nil {
		t.Fatal("accepted missing pin")
	}
	runnerRelease.sha256 = sha256Hex([]byte("runner"))
	embeddedRunnerBundle = nil
	if _, err := EnsureRunner(t.TempDir()); err == nil {
		t.Fatal("accepted missing signature")
	}
}

func TestEnsureRunnerTamperedCacheAndSidecar(t *testing.T) {
	data := []byte("signed runner bytes")
	fakeEmbed(t, data)
	dir := t.TempDir()
	path, err := EnsureRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(sha256Hex(compressedRunner)), 0o666); err != nil {
		t.Fatal(err)
	}
	path, err = EnsureRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cache not repaired with signed bytes: %q, %v", got, err)
	}
}

func TestEnsureRunnerRejectsTamperedEmbedAndCacheSymlink(t *testing.T) {
	fakeEmbed(t, []byte("signed runner bytes"))
	compressedRunner = gzipBytes(t, []byte("modified payload"))
	if _, err := EnsureRunner(t.TempDir()); err == nil {
		t.Fatal("accepted compressed payload whose final bytes miss the reviewed pin")
	}
	compressedRunner = gzipBytes(t, []byte("signed runner bytes"))
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	link := filepath.Join(dir, runnerName)
	if err := os.Symlink(filepath.Join(dir, "other"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRunner(dir); err == nil {
		t.Fatal("accepted cached symlink")
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEnsureRunnerWrongIdentityAndOfflineCache(t *testing.T) {
	fakeEmbed(t, []byte("signed runner bytes"))
	dir := t.TempDir()
	t.Setenv("BASHY_OFFLINE", "1")
	path, err := EnsureRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRunner(dir); err != nil {
		t.Fatalf("offline cache verification: %v", err)
	}
	runnerRelease.identity = "https://github.com/attacker/bashy/.github/workflows/ollama-runner.yml@refs/tags/" + testVersion
	if Available() {
		t.Fatal("wrong release identity available")
	}
	if _, err := EnsureRunner(dir); err == nil {
		t.Fatal("accepted wrong identity")
	}
	runnerRelease.identity = testIdentity
	verifyRunner = func(context.Context, string, []byte, string, binmgr.Sigstore) error {
		return fmt.Errorf("bad signature")
	}
	if _, err := EnsureRunner(dir); err == nil {
		t.Fatal("accepted cached bytes without valid signature")
	}
	if runtime.GOOS == "windows" && filepath.Ext(path) != ".exe" {
		t.Fatal("missing executable suffix")
	}
}

func TestVerifySignedBlobRejectsUnsignedSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner")
	data := []byte("binary")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	err := binmgr.VerifySignedBlob(context.Background(), path, []byte(`{"signed":"fake"}`), sha256Hex(data),
		binmgr.Sigstore{Identity: testIdentity, Issuer: binmgr.GitHubActionsIssuer})
	if err == nil {
		t.Fatal("accepted unauthenticated bundle")
	}
}
