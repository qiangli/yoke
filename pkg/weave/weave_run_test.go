// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// THE COMPATIBILITY CONTRACT, pinned.
//
// A weave queue item is now a RUN, but every script, agent and cloudbox reporter ever
// written against this tool reads the "issue" key. Day 1 means BOTH keys carry the same
// value, so nothing breaks while the vocabulary moves. Dropping "issue" is a Day-2
// decision — and this test is what makes it a DECISION rather than an accident someone
// discovers in production.
func TestEnvelopeCarriesBothRunAndIssue(t *testing.T) {
	for _, key := range []string{"run", "issue"} {
		var buf bytes.Buffer
		emitOK(&buf, weavecli.OutputJSON, "weave next", map[string]any{key: int64(7), "title": "x"})

		var got struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("emitted invalid JSON: %v\n%s", err, buf.String())
		}
		if got.Result["run"] != 7.0 {
			t.Errorf("emitting %q: result.run = %v, want 7 — the new vocabulary must always be present",
				key, got.Result["run"])
		}
		if got.Result["issue"] != 7.0 {
			t.Errorf("emitting %q: result.issue = %v, want 7 — dropping the alias breaks every existing consumer; that is a Day-2 change",
				key, got.Result["issue"])
		}
	}
}

// A caller may say --run OR --issue and reach the same variable. An agent that learned
// the old flag from a year-old README must not simply fail.
func TestRunFlagAcceptsTheDeprecatedIssueAlias(t *testing.T) {
	for _, flag := range []string{"--run", "--issue"} {
		var got int64
		cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		runFlag(cmd, &got, "the run")
		cmd.SetArgs([]string{flag, "42"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if got != 42 {
			t.Fatalf("%s did not set the run id (got %d)", flag, got)
		}
	}
}
