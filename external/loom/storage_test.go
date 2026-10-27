package loom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerStoreEndpoint(t *testing.T) {
	if got := PeerStoreEndpoint(0); got != "127.0.0.1:31890" {
		t.Fatal(got)
	}
	if got := PeerStoreBucket("PeerA"); got != "s3-peera" {
		t.Fatal(got)
	}
}

func TestEnsureConfigStorage(t *testing.T) {
	st := &Storage{Endpoint: PeerStoreEndpoint(0), Bucket: PeerStoreBucket("a"), AccessKey: "ak", SecretKey: "sk"}
	for _, existing := range []bool{false, true} {
		dir := t.TempDir()
		if existing {
			if _, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", true, nil); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", true, st)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(cfg)
		ini := string(b)
		for _, w := range []string{"[storage]", "STORAGE_TYPE = minio", "MINIO_ENDPOINT = 127.0.0.1:31890",
			"MINIO_ACCESS_KEY_ID = ak", "MINIO_SECRET_ACCESS_KEY = sk", "MINIO_BUCKET = s3-a", "MINIO_BUCKET_LOOKUP_TYPE = path", "MINIO_USE_SSL = false", "SERVE_DIRECT = true"} {
			if !strings.Contains(ini, w) {
				t.Fatalf("existing=%v missing %q in:\n%s", existing, w, ini)
			}
		}
	}
}

func TestEnsureConfigNoStorageUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", true, nil)
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "app.ini"))
	if strings.Contains(string(b), "[storage]") {
		t.Fatal("storage section written without config")
	}
}

func TestStorageFromEnv(t *testing.T) {
	t.Setenv("LOOM_S3_ENDPOINT", "")
	t.Setenv("LOOM_S3_PEER", "p")
	if s := StorageFromEnv(); s == nil || s.Endpoint != "127.0.0.1:31890" || s.Bucket != "s3-p" {
		t.Fatalf("%+v", s)
	}
	t.Setenv("LOOM_S3_ENDPOINT", "10.0.0.1:9")
	t.Setenv("LOOM_S3_BUCKET", "b")
	if s := StorageFromEnv(); s.Endpoint != "10.0.0.1:9" || s.Bucket != "b" {
		t.Fatalf("%+v", s)
	}
	t.Setenv("LOOM_S3_ENDPOINT", "")
	t.Setenv("LOOM_S3_BUCKET", "")
	t.Setenv("LOOM_S3_PEER", "")
	if StorageFromEnv() != nil {
		t.Fatal("want nil")
	}
}
