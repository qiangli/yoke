package bus

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runCoordinationCmd(t *testing.T, cmd *cobra.Command, args ...string) error {
	t.Helper()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestValidateCoordinationBodyCountsUTF8Bytes(t *testing.T) {
	if err := ValidateCoordinationBody(strings.Repeat("x", 1024)); err != nil {
		t.Fatalf("1024 ASCII bytes rejected: %v", err)
	}
	if err := ValidateCoordinationBody(strings.Repeat("é", 512)); err != nil {
		t.Fatalf("1024 multibyte UTF-8 bytes rejected: %v", err)
	}
	for _, body := range []string{strings.Repeat("x", 1025), strings.Repeat("é", 512) + "x"} {
		err := ValidateCoordinationBody(body)
		if err == nil || !strings.Contains(err.Error(), "1025 UTF-8 bytes") ||
			!strings.Contains(err.Error(), "max 1024") || !strings.Contains(err.Error(), "repo-relative path+commit") {
			t.Fatalf("1025-byte error = %v", err)
		}
	}
	if err := ValidateCoordinationBody(string([]byte{0xff})); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestMBPostAndSendRejectBeforeAppend(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(string) []string
	}{
		{name: "post", args: func(body string) []string { return []string{"post", body} }},
		{name: "send", args: func(body string) []string { return []string{"send", "target-agent", body} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			oldNames := FleetNames
			FleetNames = func() []string { return []string{"target-agent"} }
			t.Cleanup(func() { FleetNames = oldNames })
			acceptedBody := strings.Repeat("x", 1024)
			if err := runCoordinationCmd(t, NewMessageBoardCmd(), tc.args(acceptedBody)...); err != nil {
				t.Fatalf("1024-byte %s rejected: %v", tc.name, err)
			}
			before, err := Posts()
			if err != nil || len(before) != 1 {
				t.Fatalf("accepted %s append: posts=%d err=%v", tc.name, len(before), err)
			}
			if before[0].Body != acceptedBody || len(before[0].Body) != 1024 {
				t.Fatalf("accepted %s body changed: bytes=%d", tc.name, len(before[0].Body))
			}
			err = runCoordinationCmd(t, NewMessageBoardCmd(), tc.args(strings.Repeat("x", 1025))...)
			if err == nil || !strings.Contains(err.Error(), "1025 UTF-8 bytes") {
				t.Fatalf("oversized %s error = %v", tc.name, err)
			}
			after, err := Posts()
			if err != nil || len(after) != len(before) {
				t.Fatalf("rejected %s mutated board: before=%d after=%d err=%v", tc.name, len(before), len(after), err)
			}
			err = runCoordinationCmd(t, NewMessageBoardCmd(), tc.args(string([]byte{0xff}))...)
			if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
				t.Fatalf("invalid UTF-8 %s error = %v", tc.name, err)
			}
			final, err := Posts()
			if err != nil || len(final) != len(before) {
				t.Fatalf("invalid UTF-8 %s mutated board: before=%d after=%d err=%v", tc.name, len(before), len(final), err)
			}
		})
	}
}

func TestBusPublishRejectsBeforeTimelineAppend(t *testing.T) {
	isolate(t)
	if err := runCoordinationCmd(t, NewBusCmd(), "publish", "--topic", "gate", strings.Repeat("x", 1024)); err != nil {
		t.Fatalf("1024-byte publish rejected: %v", err)
	}
	before, err := watchTimeline(0)
	if err != nil || len(before) != 1 {
		t.Fatalf("accepted publish timeline=%d err=%v", len(before), err)
	}
	err = runCoordinationCmd(t, NewBusCmd(), "publish", "--topic", "gate", strings.Repeat("x", 1025))
	if err == nil || !strings.Contains(err.Error(), "1025 UTF-8 bytes") {
		t.Fatalf("oversized publish error = %v", err)
	}
	after, err := watchTimeline(0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("rejected publish mutated timeline: before=%d after=%d err=%v", len(before), len(after), err)
	}
}
