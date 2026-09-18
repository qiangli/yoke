package gcloud

import (
	"context"
	"strings"
	"testing"
)

func TestInstallHintNonEmptyAndVendorPath(t *testing.T) {
	h := InstallHint()
	if strings.TrimSpace(h) == "" {
		t.Fatal("InstallHint returned empty")
	}
	if !strings.Contains(h, "cloud.google.com/sdk") {
		t.Errorf("InstallHint should point at the vendor installer, got %q", h)
	}
}

func TestResolveDoesNotAutoDownload(t *testing.T) {
	// bashy must NOT TOFU-download the Cloud SDK — Resolve returns an actionable
	// install hint, never a Tool. (The host copy is used first via PreferHost.)
	_, err := Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("Resolve should error rather than auto-download the Cloud SDK")
	}
	if !strings.Contains(err.Error(), "gcloud not found") || !strings.Contains(err.Error(), "cloud.google.com/sdk") {
		t.Errorf("Resolve error should explain + carry the install hint, got %v", err)
	}
}
