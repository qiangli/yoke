package binmgr

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstEncode(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// A single-file .zst asset (tuwunel's release format) is verified against the
// digest of the COMPRESSED download, then decompressed into place 0755; any
// failure leaves nothing installed.
func TestEnsure_SingleFileZst(t *testing.T) {
	exe := []byte("#!/bin/sh\necho tuwunel\n" + strings.Repeat("x", 4096))
	good := zstEncode(t, exe)
	corrupt := append([]byte(nil), good[:len(good)/2]...)

	cases := []struct {
		name    string
		body    []byte
		sha     string
		limit   int64
		wantErr string
	}{
		{"sha match installs", good, sha256hex(good), 512 << 20, ""},
		{"sha mismatch refused", good, sha256hex(exe), 512 << 20, "sha256 mismatch"},
		{"truncated zstd", corrupt, sha256hex(corrupt), 512 << 20, "zstd"},
		{"over cap refused", good, sha256hex(good), int64(len(exe) - 1), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()
			cache := t.TempDir()
			t.Setenv("BASHY_BIN_CACHE", cache)
			old := maxDecompressedSize
			maxDecompressedSize = tc.limit
			defer func() { maxDecompressedSize = old }()

			tool := toolFor(srv.URL+"/v1.9.4-release-tuwunel.zst", tc.sha, "")
			path, err := Ensure(context.Background(), tool)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, exe) {
					t.Fatalf("installed content is not the decompressed original")
				}
				if !isExecutable(path) {
					t.Fatalf("installed binary is not executable: %s", path)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			var left []string
			_ = filepath.Walk(cache, func(p string, info os.FileInfo, _ error) error {
				if info != nil && !info.IsDir() {
					left = append(left, p)
				}
				return nil
			})
			if len(left) != 0 {
				t.Fatalf("failed install left files behind: %v", left)
			}
		})
	}
}
