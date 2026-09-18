package engine

import (
	"testing"
)

func TestSandboxDockerfileEmbedded(t *testing.T) {
	if len(sandboxDockerfile) == 0 {
		t.Fatal("embedded Dockerfile.sandbox is empty")
	}

	content := string(sandboxDockerfile)

	// Verify key contents.
	if !containsStr(content, "FROM") {
		t.Error("Dockerfile missing FROM instruction")
	}
	if !containsStr(content, "bash") {
		t.Error("Dockerfile should include bash")
	}
	if !containsStr(content, "git") {
		t.Error("Dockerfile should include git")
	}
	if !containsStr(content, "agent") {
		t.Error("Dockerfile should create agent user")
	}
	if !containsStr(content, "/workspace") {
		t.Error("Dockerfile should set up /workspace")
	}
}

func TestImageInfo(t *testing.T) {
	img := ImageInfo{
		ID:         "sha256:abc123",
		Repository: "ubuntu",
		Tag:        "24.04",
		Size:       78000000,
	}
	if img.Repository != "ubuntu" {
		t.Error("unexpected repository")
	}
	if img.Tag != "24.04" {
		t.Error("unexpected tag")
	}
}

func containsStr(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && // prevent trivial matches
		stringContains(s, substr)
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
