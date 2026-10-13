package kopia

import (
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/internal/serviceenv"
)

// `kopia server start` rejects repository-only flags such as
// --no-check-for-updates (v0.23.1); updates are disabled via the env var.
func TestServerArgvAndEnvPrivacy(t *testing.T) {
	args := serverArgs("/d/kopia.config", "127.0.0.1:51515")
	for _, a := range append(append([]string{}, args...), kopiaPrivacyArgs()...) {
		if strings.Contains(a, "check-for-updates") {
			t.Fatalf("repository-only flag %q in argv %v", a, args)
		}
	}
	for _, want := range []string{"--otlp-trace", "server", "start", "--address=127.0.0.1:51515"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %s in %v", want, args)
		}
	}
	env := kopiaEnv("pw")
	for _, want := range []string{"KOPIA_CHECK_FOR_UPDATES=false", "KOPIA_PASSWORD=pw"} {
		if !slices.Contains(env, want) {
			t.Fatalf("missing %s in %v", want, env)
		}
	}
}

// Kopia v0.23.1 uses otlptracegrpc (gRPC only). Its child env must target
// the loopback gRPC receiver (127.0.0.1:31416) with grpc protocol, not the
// OTLP/HTTP proxy, or traces are silently lost.
func TestKopiaOTLPEnvTargetsGRPCReceiver(t *testing.T) {
	env := serviceenv.LocalOTLP([]string{"KOPIA_PASSWORD=secret", "OTEL_EXPORTER_OTLP_ENDPOINT=https://vendor.example"}, "kopia")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "vendor.example") {
		t.Fatalf("vendor endpoint survived:\n%s", joined)
	}
	for _, want := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + serviceenv.LocalCollectorGRPCEndpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc",
		"OTEL_SERVICE_NAME=kopia",
	} {
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in kopia OTLP env:\n%s", want, joined)
		}
	}
	// Must not contain http/protobuf for kopia; that would indicate the
	// HTTP proxy, which kopia's gRPC exporter cannot speak to.
	if strings.Contains(joined, "http/protobuf") {
		t.Fatalf("kopia env must not contain http/protobuf (needs grpc):\n%s", joined)
	}
	// Ensure the gRPC address is loopback-only (never vendor, never 0.0.0.0).
	for _, e := range env {
		if strings.Contains(e, "OTEL_EXPORTER_OTLP_ENDPOINT=") && strings.Contains(e, "0.0.0.0") {
			t.Fatalf("non-loopback bind in kopia env: %q", e)
		}
	}
	if !slices.Contains(kopiaPrivacyArgs(), "--otlp-trace") {
		t.Fatalf("--otlp-trace missing: gRPC receiver would never be exercised")
	}
}

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "kopia/kopia" || s.Name != "kopia" || s.Version != "latest" {
		t.Fatalf("default spec = %+v", s)
	}
	if s.Member != "kopia" {
		t.Fatalf("archive member should be the kopia binary, got %q", s.Member)
	}
	if s.AssetMatch == nil {
		t.Fatal("kopia spec needs a custom AssetMatch (macOS/x64 tokens)")
	}
	if Spec("v0.18.0").Version != "v0.18.0" {
		t.Fatal("version override not honored")
	}
}

// Kopia's release tokens differ from Go's: darwin→macOS, amd64→x64.
func TestAssetMatch_MapsTokens(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"kopia-0.18.0-linux-x64.tar.gz", "linux", "amd64", true},    // amd64→x64
		{"kopia-0.18.0-macOS-arm64.tar.gz", "darwin", "arm64", true}, // darwin→macOS
		{"kopia-0.18.0-macOS-x64.tar.gz", "darwin", "amd64", true},
		{"kopia-0.18.0-windows-x64.zip", "windows", "amd64", true},
		{"kopia-0.18.0-linux-arm64.tar.gz", "darwin", "arm64", false}, // wrong os
		{"kopia-0.18.0-linux-x64.tar.gz", "linux", "arm64", false},    // wrong arch
	}
	for _, c := range cases {
		if got := assetMatch(c.name, c.goos, c.goarch); got != c.want {
			t.Errorf("assetMatch(%q, %s/%s)=%v, want %v", c.name, c.goos, c.goarch, got, c.want)
		}
	}
}
