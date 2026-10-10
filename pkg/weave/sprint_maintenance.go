package weave

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

// The fixed identity lets maintenance stories travel without treating the
// reserved local number as a different initiative on each host. Leases and
// threads remain host-local, exactly like the rest of the sprint board.
var maintenanceUUID = uuid.NewSHA1(sprintUUIDNamespace, []byte("bashy/standing-maintenance/v1")).String()

func isMaintenance(s *weaveStory) bool {
	return s != nil && s.ID == todopkg.MaintenanceSprint && s.UUID == maintenanceUUID
}

func nextOrdinarySprintID(q *weaveQueue) int64 {
	if q.NextStoryID < 1 {
		q.NextStoryID = 1
	}
	for q.NextStoryID == todopkg.MaintenanceSprint || findWeaveStory(q, q.NextStoryID) != nil {
		q.NextStoryID++
	}
	id := q.NextStoryID
	q.NextStoryID++
	return id
}

func prepareMaintenance(st *issue.Store) (string, string, error) {
	if st.Sub != todopkg.RepoSub {
		return "", "", fmt.Errorf("maintenance stories use a repository todo store; use --repo or --base-dir, or keep this personal todo unlinked")
	}
	root, err := normalizeStoryRoot(st.Root)
	if err != nil {
		return "", "", err
	}
	return ensureMaintenance(root)
}

// ensureMaintenance reserves the default card without changing ordinary
// lifecycle state. In particular, filing a story never reopens a done sprint.
func ensureMaintenance(root string) (string, string, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return "", "", err
	}
	// Existing-card reads stay read-only. Recheck under the lock when
	// creating a card or attaching a new store.
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return "", "", err
	}
	if s := findWeaveStory(q, todopkg.MaintenanceSprint); s != nil {
		if !isMaintenance(s) {
			return "", "", fmt.Errorf("sprint #9999 is occupied by an ordinary card; resolve that conflict before using maintenance (nothing overwritten)")
		}
		if root == "" {
			return s.UUID, s.Title, nil
		}
		for _, have := range s.StoryRoots {
			if filepath.Clean(have) == root {
				return s.UUID, s.Title, nil
			}
		}
	}
	var title string
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, todopkg.MaintenanceSprint)
		if s != nil && !isMaintenance(s) {
			return fmt.Errorf("sprint #9999 is occupied by an ordinary card; resolve that conflict before using maintenance (nothing overwritten)")
		}
		if s == nil {
			now := time.Now().UTC()
			s = &weaveStory{ID: todopkg.MaintenanceSprint, UUID: maintenanceUUID,
				Title: "Maintenance", Epic: "maintenance", Column: "backlog",
				PrimaryGoal: "Deliver independent one-off fixes with story-level verification",
				SpecRef:     "builtin:maintenance",
				Acceptance:  "Each story records its problem, expected behavior, verification and delivery evidence.",
				Continuity:  "Default sprint for unrelated one-off fixes. Read each story before working. Use the normal sprint lifecycle: take the manager seat explicitly, move to doing before claims, submit evidence, verify and accept. Handoff or end when appropriate. Existing completed stories stay closed when this sprint is reopened.",
				Execution:   sprintExecution{PriorityFirst: true}, Created: now, UpdatedAt: now}
			weaveStoryAppend(s, "conductor", kindStage, "created default maintenance sprint")
			q.Stories = append(q.Stories, s)
			mintSprintHandles(q)
		}
		title = s.Title
		if root == "" {
			return nil
		}
		for _, have := range s.StoryRoots {
			if filepath.Clean(have) == root {
				return nil
			}
		}
		s.StoryRoots = append(s.StoryRoots, root)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return maintenanceUUID, title, nil
}

// Only commands that address a sprint get a default. Board/list/create verbs
// and unrelated filters retain their existing meanings. Expansion wraps the
// entire runner, including lease authorization and host-supplied adapters.
func installDefaultSprintArgs(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		installDefaultSprintArgs(cmd)
		words := strings.Fields(cmd.Use)
		if cmd.RunE == nil || len(words) < 2 || (words[1] != "<sprint>" && words[1] != "N") {
			continue
		}
		required := 1
		for _, word := range words[2:] {
			if strings.HasPrefix(word, "--") || strings.HasPrefix(word, "[") {
				break
			}
			if strings.HasPrefix(word, "<") {
				required++
			}
		}
		normalize := func(args []string) []string {
			omitted := len(args) == required-1
			if omitted {
				return append([]string{strconv.FormatInt(todopkg.MaintenanceSprint, 10)}, args...)
			}
			return args
		}
		validate, run := cmd.Args, cmd.RunE
		cmd.Args = func(c *cobra.Command, args []string) error {
			if validate == nil {
				return nil
			}
			return validate(c, normalize(args))
		}
		cmd.RunE = func(c *cobra.Command, args []string) error {
			args = normalize(args)
			if len(args) > 0 && (args[0] == "9999" || args[0] == "#9999" || args[0] == maintenanceUUID) {
				if _, _, err := ensureMaintenance(""); err != nil {
					return err
				}
			}
			return run(c, args)
		}
		cmd.Use = strings.Replace(cmd.Use, words[1], "[sprint]", 1)
		if cmd.Long == "" {
			cmd.Long = cmd.Short
		}
		cmd.Long += "\n\nOmitted sprint defaults to #9999 Maintenance. Explicit sprint arguments take precedence; normal lifecycle and authorization rules apply."
	}
}
