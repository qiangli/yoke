package weave

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/qiangli/yoke/pkg/room"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSprintLeaseTokenAuthorization(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "agent-b")
	raw, hash, err := mintSprintLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		t.Fatal("token is not 32 random bytes")
	}
	sum := sha256.Sum256([]byte(raw))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("incorrect hash")
	}
	for _, tc := range []struct {
		name, token, mode, kind        string
		override, grandfather, refused bool
	}{
		{name: "correct", token: raw, mode: "must"},
		{name: "wrong should", token: "wrong", mode: "should", kind: "bypass"},
		{name: "missing default", kind: "bypass"},
		{name: "wrong must", token: "wrong", mode: "must", refused: true},
		{name: "missing must", mode: "must", refused: true},
		{name: "grandfather", mode: "must", grandfather: true},
		{name: "override", mode: "must", override: true, kind: "override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASHY_SPRINT_LEASE_TOKEN", tc.token)
			t.Setenv("BASHY_SPRINT_ENFORCE", tc.mode)
			s := &weaveStory{ID: 7, Lease: &weaveStoryLease{Holder: "agent-a", At: time.Now(), TokenHash: hash}}
			if tc.grandfather {
				s.Lease.TokenHash = ""
			}
			cmd := &cobra.Command{}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			cmd.Flags().Bool("override", tc.override, "")
			cmd.Flags().String("reason", "operator recovery", "")
			err := authorizeSprintLeaseToken(cmd, s, "checkpoint")
			if (err != nil) != tc.refused {
				t.Fatalf("err=%v", err)
			}
			if tc.kind != "" {
				if len(s.Thread) != 1 || s.Thread[0].Kind != tc.kind || s.Thread[0].Author != "agent-b" || !strings.Contains(s.Thread[0].Body, "checkpoint") || s.Thread[0].At.IsZero() {
					t.Fatalf("thread=%+v", s.Thread)
				}
				if tc.kind == "bypass" && !strings.Contains(stderr.String(), "detected bypass: checkpoint without the lease token for sprint 7") {
					t.Fatal(stderr.String())
				}
				if tc.kind == "override" && !strings.Contains(s.Thread[0].Body, "operator recovery") {
					t.Fatal("missing reason")
				}
			} else if len(s.Thread) != 0 || stderr.Len() != 0 {
				t.Fatalf("unexpected audit: %v %s", s.Thread, stderr.String())
			}
			data, _ := json.Marshal(s)
			if strings.Contains(string(data), raw) {
				t.Fatal("raw token leaked into card/thread")
			}
		})
	}
}

func TestSprintLeaseTokenCLIAndHeartbeat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv("BASHY_PRINCIPAL", "agent-a")
	seedAgent(t, "agent-a")
	if out, code := runSprint(t, "add", "token fixture"); code != 0 {
		t.Fatal(out)
	}
	out, code := runSprint(t, "take", "1", "--owner", "agent-a")
	if code != 0 {
		t.Fatal(out)
	}
	s, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Lease.TokenHash == "" {
		t.Fatal("no hash minted")
	}
	path, err := sprintLeaseTokenPath(s.ID, s.Lease.Holder)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(data))
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 || len(raw) != 64 || !strings.Contains(out, raw) {
		t.Fatal("token delivery or permissions incorrect")
	}
	out, code = runSprint(t, "checkpoint", "1", "-m", "still working")
	if code != 0 || strings.Contains(out, raw) {
		t.Fatalf("checkpoint: %d %s", code, out)
	}
	after, _ := sprintOwnerSnapshot(1)
	if after.Lease.TokenHash != s.Lease.TokenHash {
		t.Fatal("heartbeat discarded token")
	}
	out, code = runSprint(t, "take", "1", "--owner", "agent-a")
	if code != 0 || strings.Contains(out, raw) {
		t.Fatalf("retake disclosed token: %d %s", code, out)
	}
	after, _ = sprintOwnerSnapshot(1)
	if after.Lease.TokenHash != s.Lease.TokenHash {
		t.Fatal("retake rotated active manager credential")
	}
	t.Setenv("BASHY_PRINCIPAL", "agent-b")
	if out, code = runSprint(t, "checkpoint", "1", "-m", "bypass"); code == 0 {
		t.Fatalf("other seat reused file: %s", out)
	}
	if out, code = runSprint(t, "checkpoint", "1", "-m", "override", "--override", "--reason", "recovery"); code != 0 {
		t.Fatal(out)
	}
	after, _ = sprintOwnerSnapshot(1)
	found := false
	for _, e := range after.Thread {
		if e.Kind == "override" {
			found = true
		}
	}
	if !found {
		t.Fatal("override not persisted")
	}
	t.Setenv("BASHY_SPRINT_ENFORCE", "should")
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "wrong")
	stdout, stderr, code, _ := runSprintStreams(t, "checkpoint", "1", "-m", "warn and continue")
	if code != 0 || !strings.Contains(stderr, "detected bypass: checkpoint without the lease token for sprint 1") {
		t.Fatalf("should: %d %s %s", code, stdout, stderr)
	}
	after, _ = sprintOwnerSnapshot(1)
	found = false
	for _, e := range after.Thread {
		if e.Kind == "bypass" && e.Author == "agent-b" {
			found = true
		}
	}
	if !found || after.Continuity != "warn and continue" {
		t.Fatal("bypass audit or mutation not persisted")
	}
	card, _ := json.Marshal(after)
	if strings.Contains(string(card), raw) {
		t.Fatal("raw token persisted on card")
	}
}

func TestSprintLeaseTokenManagedLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "agent-a")
	seedAgent(t, "agent-a")
	if out, code := runSprint(t, "add", "managed token fixture"); code != 0 {
		t.Fatal(out)
	}
	var tokens []string
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		tokens = append(tokens, req.Env[sprintLeaseTokenEnv])
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "managed-seat", Reused: len(tokens) > 1, Transport: room.TransportManaged}, nil
	})
	out, code := runSprint(t, "start", "1", "--owner", "agent-a", "--instruction", "deliver")
	if code != 0 {
		t.Fatal(out)
	}
	if len(tokens) != 1 || len(tokens[0]) != 64 {
		t.Fatal("no launch token")
	}
	s, _ := sprintOwnerSnapshot(1)
	if s.Lease.TokenHash != sprintLeaseTokenHash(tokens[0]) {
		t.Fatal("launch token does not match lease")
	}
	card, _ := json.Marshal(s)
	if strings.Contains(out, tokens[0]) || strings.Contains(string(card), tokens[0]) {
		t.Fatal("managed token disclosed")
	}
	if out, code = runSprint(t, "instruct", "1", "--instruction", "continue"); code != 0 {
		t.Fatal(out)
	}
	if len(tokens) != 2 || tokens[1] != tokens[0] {
		t.Fatal("instruct rotated an active token")
	}
}

func TestSprintLeaseTokenGuardCoverage(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "wrong")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	dir, _ := sprintStoreDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{ID: 1, Lease: &weaveStoryLease{Holder: "agent-a", TokenHash: sprintLeaseTokenHash("secret")}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var visit func(*cobra.Command)
	visit = func(root *cobra.Command) {
		for _, cmd := range root.Commands() {
			if cmd.Name() == "goal" {
				visit(cmd)
				continue
			}
			if cmd.Flags().Lookup("reason") == nil || cmd.Flags().Lookup("override") == nil || cmd.RunE == nil {
				continue
			}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			// Invoke the wrapped entry directly so required flags and positional
			// arguments cannot mask a missing authorization guard.
			if err := cmd.RunE(cmd, []string{"1", "unused"}); err == nil || !strings.Contains(stderr.String(), "detected bypass") {
				t.Errorf("%s: %v", cmd.CommandPath(), err)
			}
		}
	}
	visit(NewSprintCmd())
}

func TestSprintLeaseTokenWorkerVerbsUngated(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "wrong")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	dir, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{ID: 1, Lease: &weaveStoryLease{Holder: "agent-a", TokenHash: sprintLeaseTokenHash("secret")}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*cobra.Command{}
	for _, cmd := range NewSprintCmd().Commands() {
		if cmd.Name() == "goal" {
			continue
		}
		byName[cmd.Name()] = cmd
	}
	// Worker verbs on a story are attributed to the claiming agent and must
	// not require the manager's lease token, even in must mode.
	for _, name := range []string{"claim", "yield", "submit"} {
		cmd, ok := byName[name]
		if !ok || cmd.RunE == nil {
			t.Fatalf("%s command not found", name)
		}
		if cmd.Flags().Lookup("override") != nil || cmd.Flags().Lookup("reason") != nil {
			t.Fatalf("%s still carries the lease-guard flags", name)
		}
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		// Invoke the entry directly so the guard, if present, fires before
		// the verb's own argument handling.
		err := cmd.RunE(cmd, []string{"1", "unused"})
		if err != nil && (strings.Contains(err.Error(), "detected bypass") || strings.Contains(err.Error(), "lease token")) {
			t.Fatalf("%s refused without the manager token: %v", name, err)
		}
		if strings.Contains(stderr.String(), "detected bypass") {
			t.Fatalf("%s warned about a bypass: %s", name, stderr.String())
		}
	}
	s, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Thread {
		if e.Kind == "bypass" {
			t.Fatalf("worker verb recorded a bypass event: %+v", e)
		}
	}
	// The manager's accept verb stays gated.
	accept, ok := byName["accept"]
	if !ok || accept.RunE == nil {
		t.Fatal("accept command not found")
	}
	if accept.Flags().Lookup("override") == nil || accept.Flags().Lookup("reason") == nil {
		t.Fatal("accept lost its lease-guard flags")
	}
	var stderr bytes.Buffer
	accept.SetErr(&stderr)
	if err := accept.RunE(accept, []string{"1", "unused"}); err == nil || !strings.Contains(stderr.String(), "detected bypass") {
		t.Fatalf("accept no longer requires the lease token: err=%v stderr=%s", err, stderr.String())
	}
}

func TestSprintLeaseTokenInstructionAcquiresLegacyLease(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", "")
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir, _ := sprintStoreDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{ID: 1, Owner: "agent-a", Lease: &weaveStoryLease{Holder: "agent-a", At: time.Now()}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var raw string
	withSprintOwnerLauncher(t, func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		raw = req.Env[sprintLeaseTokenEnv]
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: "managed-seat", Transport: room.TransportManaged}, nil
	})
	if _, _, err := ensureSprintOwnerSession(context.Background(), 1, "agent-a", "continue", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	s, _ := sprintOwnerSnapshot(1)
	if len(raw) != 64 || s.Lease.TokenHash != sprintLeaseTokenHash(raw) {
		t.Fatal("instruction launch did not acquire a token")
	}
}
