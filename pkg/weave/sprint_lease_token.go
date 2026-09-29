package weave

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const sprintLeaseTokenEnv = "BASHY_SPRINT_LEASE_TOKEN"

// sprintLeaseGatedVerbs are manager state changes. Goal subverbs are also
// gated because they are all state changes on the sprint plan.
var sprintLeaseGatedVerbs = map[string]bool{
	"accept":     true,
	"fail":       true,
	"assign":     true,
	"checkpoint": true,
	"move":       true,
	"edit":       true,
	"extend":     true,
	"end":        true,
	"stop":       true,
	"handoff":    true,
	"link":       true,
	"unlink":     true,
	"track":      true,
	"untrack":    true,
	"focus":      true,
	"advance":    true,
	"rm":         true,
}

func mintSprintLeaseToken() (string, string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	raw := hex.EncodeToString(secret[:])
	return raw, sprintLeaseTokenHash(raw), nil
}

func sprintLeaseTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func sprintLeaseTokenPath(id int64, holder string) (string, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return "", err
	}
	// Hash the address so an agent name can never become a path component.
	return filepath.Join(dir, fmt.Sprintf("lease-%d-%s.token", id, sprintLeaseTokenHash(holder))), nil
}

func saveSprintLeaseToken(id int64, holder, raw string) error {
	path, err := sprintLeaseTokenPath(id, holder)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".lease-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readSprintLeaseToken(id int64, holder string) string {
	path, err := sprintLeaseTokenPath(id, holder)
	if err != nil {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func authorizeSprintLeaseToken(cmd *cobra.Command, s *weaveStory, verb string) error {
	override, _ := cmd.Flags().GetBool("override")
	// focus already has a string-valued priority override. Preserve that API.
	if f := cmd.Flags().Lookup("override"); f != nil && f.Value.Type() == "string" {
		override = f.Value.String() != ""
	}
	if override {
		reason, _ := cmd.Flags().GetString("reason")
		if f := cmd.Flags().Lookup("override"); f != nil && f.Value.Type() == "string" && reason == "" {
			reason = f.Value.String()
		}
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--override requires --reason for sprint %d", s.ID)
		}
		weaveStoryAppend(s, weaveConductorName(""), "override", verb+": "+reason)
		return nil
	}
	if s.Lease == nil || s.Lease.TokenHash == "" {
		return nil
	}
	raw := os.Getenv(sprintLeaseTokenEnv)
	if raw == "" {
		actor, ok := weaveConductorIdentity("")
		if ok && actor == s.Lease.Holder {
			raw = readSprintLeaseToken(s.ID, s.Lease.Holder)
		}
	}
	if raw != "" && subtle.ConstantTimeCompare([]byte(sprintLeaseTokenHash(raw)), []byte(s.Lease.TokenHash)) == 1 {
		return nil
	}
	msg := fmt.Sprintf("detected bypass: %s without the lease token for sprint %d", verb, s.ID)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BASHY_SPRINT_ENFORCE")), "must") {
		return fmt.Errorf("%s; set %s to the manager's token, or use --override --reason <reason>", msg, sprintLeaseTokenEnv)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), msg)
	weaveStoryAppend(s, weaveConductorName(""), "bypass", verb)
	return nil
}

// Install at command entry, before gates, story writes, or manager shutdowns.
// Audit entries are committed independently so a later failed operation does
// not erase evidence that an override or bypass was attempted.
func installSprintLeaseTokenGuards(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		if cmd.Name() == "goal" {
			installSprintLeaseTokenGuards(cmd)
			continue
		}
		gated := root.Name() == "goal" || sprintLeaseGatedVerbs[cmd.Name()]
		// claim, yield and submit are worker verbs attributed to the
		// claiming agent, not manager state verbs, so they stay ungated.
		if !gated || cmd.RunE == nil {
			continue
		}
		if cmd.Flags().Lookup("override") == nil {
			cmd.Flags().Bool("override", false, "operator override of lease authorization (requires --reason)")
		}
		if cmd.Flags().Lookup("reason") == nil {
			cmd.Flags().String("reason", "", "reason for the operator override")
		}
		run := cmd.RunE
		verb := cmd.Name()
		if root.Name() == "goal" {
			verb = "goal " + verb
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return run(cmd, args)
			}
			dir, err := sprintStoreDir()
			if err != nil {
				return err
			}
			err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
				s, err := findSprintByHandle(q, args[0])
				if err != nil {
					// Leave argument and missing-card diagnostics to the verb.
					return nil
				}
				return authorizeSprintLeaseToken(cmd, s, verb)
			})
			if err != nil {
				return err
			}
			return run(cmd, args)
		}
	}
}

// An instruction may be the first managed launch of an older CLI lease.
// Persist the credential before invoking the host, so the manager can use it
// immediately. Reusing an active session must not rotate its credential.
func sprintInstructionLeaseToken(id int64, owner string) (string, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return "", err
	}
	var raw string
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if s.Lease == nil || s.Lease.Holder != owner {
			return fmt.Errorf("sprint #%d has no lease for %s; take the lease first", id, owner)
		}
		if s.Lease.TokenHash != "" {
			raw = readSprintLeaseToken(id, owner)
			if raw == "" || sprintLeaseTokenHash(raw) != s.Lease.TokenHash {
				return fmt.Errorf("sprint #%d lease token file is unavailable; take the lease again before launching its manager", id)
			}
			return nil
		}
		var hash string
		var err error
		raw, hash, err = mintSprintLeaseToken()
		if err != nil {
			return err
		}
		if err = saveSprintLeaseToken(id, owner, raw); err != nil {
			return err
		}
		s.Lease.TokenHash = hash
		return nil
	})
	return raw, err
}

// Re-taking one's current seat is a heartbeat, not a new acquisition. Keep
// the active process's credential valid and never disclose it a second time.
func prepareSprintLeaseToken(s *weaveStory, owner string) (raw, hash string, minted bool, err error) {
	if s.Lease != nil && s.Lease.Holder == owner && s.Lease.TokenHash != "" {
		raw = readSprintLeaseToken(s.ID, owner)
		if raw != "" && sprintLeaseTokenHash(raw) == s.Lease.TokenHash {
			return raw, s.Lease.TokenHash, false, nil
		}
		return "", "", false, fmt.Errorf("sprint #%d lease token file is unavailable; handoff with --override --reason <reason>, then take the lease again", s.ID)
	}
	raw, hash, err = mintSprintLeaseToken()
	return raw, hash, true, err
}
