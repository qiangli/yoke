package weave

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// nextSprintID allocates the next free sprint number on the board.
func nextSprintID(q *weaveQueue) int64 {
	if q.NextStoryID < 1 {
		q.NextStoryID = 1
	}
	for findWeaveStory(q, q.NextStoryID) != nil {
		q.NextStoryID++
	}
	id := q.NextStoryID
	q.NextStoryID++
	return id
}

// defaultSprint is THE selector for an omitted sprint operand: the most
// recently created existing card (ties go to the higher number). Change the
// policy here and every default-taking verb and `todo add` follows. It never
// creates a card; an empty board is an error.
func defaultSprint(q *weaveQueue) (*weaveStory, error) {
	var best *weaveStory
	for _, s := range q.Stories {
		if best == nil || s.Created.After(best.Created) || (s.Created.Equal(best.Created) && s.ID > best.ID) {
			best = s
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no sprint exists to default to; create one with `bashy sprint add`, or pass an explicit sprint (todo add: --sprint N or --no-sprint)")
	}
	return best, nil
}

func defaultSprintNumber() (int64, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return 0, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return 0, err
	}
	s, err := defaultSprint(q)
	if err != nil {
		return 0, err
	}
	return s.ID, nil
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
		var selection string
		cmd.Flags().StringVar(&selection, "sprint", "", "select a sprint number or last; alternative to the sprint operand")
		normalize := func(args []string) ([]string, error) {
			if cmd.Flags().Changed("sprint") {
				if len(args) != required-1 {
					return nil, fmt.Errorf("use either --sprint or a sprint operand, not both")
				}
				if selection == "" {
					return nil, fmt.Errorf("--sprint requires a sprint number or last")
				}
				args = append([]string{selection}, args...)
			}
			omitted := len(args) == required-1
			last := !omitted && len(args) > 0 && args[0] == "last"
			if !omitted && !last {
				return args, nil
			}
			n, err := defaultSprintNumber()
			if err != nil {
				return nil, err
			}
			if last {
				args = args[1:]
			}
			return append([]string{strconv.FormatInt(n, 10)}, args...), nil
		}
		validate, run := cmd.Args, cmd.RunE
		cmd.Args = func(c *cobra.Command, args []string) error {
			args, err := normalize(args)
			if err != nil {
				return err
			}
			if validate == nil {
				return nil
			}
			return validate(c, args)
		}
		cmd.RunE = func(c *cobra.Command, args []string) error {
			args, err := normalize(args)
			if err != nil {
				return err
			}
			return run(c, args)
		}
		cmd.Use = strings.Replace(cmd.Use, words[1], "[sprint]", 1)
		if cmd.Long == "" {
			cmd.Long = cmd.Short
		}
		cmd.Long += "\n\nUse a sprint number or last. Omitted sprint defaults to the most recently created existing sprint (an empty board is an error; nothing is created). Explicit sprint arguments take precedence; normal lifecycle and authorization rules apply."
	}
}
