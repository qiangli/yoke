//go:build windows

package foreman

import (
	"context"
	"strings"
	"testing"
)

func TestManagedControlFailsTruthfullyOnNativeWindows(t *testing.T) {
	if ControlSupported() {
		t.Fatal("native Windows unexpectedly advertises Unix-socket control")
	}
	s, err := Start(context.Background(), Options{ID: "windows-control", Goal: "test", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ServeControl(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("ServeControl error = %v", err)
	}
	if _, err := SendCommand(t.TempDir(), "windows-control", Command{Verb: CommandStop}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("SendCommand error = %v", err)
	}
}
