package kb

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var shareableWriteCheck = defaultShareableWriteCheck

func newNoteCmd(dir, ring *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "note",
		Short: "Write note-form knowledge records",
	}
	cmd.AddCommand(newNoteAddCmd(dir, ring))
	return cmd
}

func newNoteAddCmd(dir, ring *string) *cobra.Command {
	var (
		candidate      bool
		episode        string
		title, desc    string
		body, bodyFile string
		force          bool
		typ            string
		tags           []string
		slug           string
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a candidate note",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !candidate {
				return fmt.Errorf("kb: note add requires --candidate")
			}
			if strings.TrimSpace(title) == "" {
				return fmt.Errorf("kb: note add requires --title")
			}
			if !ValidType(typ) {
				return fmt.Errorf("kb: invalid type %q (lesson|gotcha|runbook|decision|fact)", typ)
			}
			text, err := noteBody(c, body, bodyFile)
			if err != nil {
				return err
			}
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("kb: note add requires --body or -f")
			}
			effRing := strings.TrimSpace(*ring)
			if effRing != "agent" {
				if err := shareableWriteCheck(title + "\n" + desc + "\n" + text); err != nil {
					return err
				}
			}
			store := openRing(*dir, *ring)
			noteSlug := strings.TrimSpace(slug)
			if noteSlug == "" {
				noteSlug = Slugify(title)
			}
			p := &Page{
				Slug:        noteSlug,
				Form:        FormNote,
				Type:        typ,
				Title:       strings.TrimSpace(title),
				Description: strings.TrimSpace(desc),
				Tags:        tags,
				Status:      StatusCandidate,
				Source:      &Source{Tool: ToolID(), Host: HostID(), Episode: strings.TrimSpace(episode)},
				Body:        strings.TrimSpace(text),
			}
			if p.Source.Episode == "" {
				p.Source.Episode = EpisodeID()
			}
			pages, err := store.List()
			if err != nil {
				return err
			}
			if !force {
				if dup := NearDuplicate(pages, p.Title, p.Description); dup != nil {
					return fmt.Errorf("kb: looks like a duplicate of %q (%s) - use 'kb update %s' or 'kb supersede %s' (or --force)",
						dup.Title, dup.Slug, dup.Slug, dup.Slug)
				}
			}
			if err := store.Write(p, "note-add"); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added note %s (candidate)\n", p.Slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&candidate, "candidate", false, "write the note as a candidate")
	cmd.Flags().StringVar(&episode, "episode", "", "episode id to stamp on the note")
	cmd.Flags().StringVar(&typ, "type", TypeLesson, "note type: lesson|gotcha|runbook|decision|fact")
	cmd.Flags().StringVar(&title, "title", "", "note title")
	cmd.Flags().StringVar(&desc, "description", "", "optional routing description")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "tags")
	cmd.Flags().StringVar(&slug, "slug", "", "override the auto slug")
	cmd.Flags().StringVar(&body, "body", "", "note body text")
	cmd.Flags().StringVarP(&bodyFile, "file", "f", "", "read the body from FILE ('-' = stdin)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the duplicate check")
	return cmd
}

func noteBody(c *cobra.Command, body, bodyFile string) (string, error) {
	if strings.TrimSpace(bodyFile) == "" {
		return body, nil
	}
	if bodyFile == "-" {
		b, err := io.ReadAll(c.InOrStdin())
		return string(b), err
	}
	b, err := os.ReadFile(bodyFile)
	return string(b), err
}

func defaultShareableWriteCheck(s string) error {
	for _, v := range secretValues() {
		if v != "" && strings.Contains(s, v) {
			return fmt.Errorf("kb: refusing shareable-ring write containing a secret value")
		}
	}
	if h := strings.TrimSpace(HostID()); h != "" && containsHostToken(s, h) {
		return fmt.Errorf("kb: refusing shareable-ring write containing host identity %q; use --ring agent", h)
	}
	if h, err := os.Hostname(); err == nil {
		h = strings.TrimSpace(h)
		if h != "" && containsHostToken(s, h) {
			return fmt.Errorf("kb: refusing shareable-ring write containing host identity %q; use --ring agent", h)
		}
	}
	return nil
}

func secretValues() []string {
	out := []string{}
	for _, env := range os.Environ() {
		k, v, ok := strings.Cut(env, "=")
		if !ok || len(v) < 8 || !looksSecretName(k) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func looksSecretName(k string) bool {
	k = strings.ToUpper(k)
	for _, needle := range []string{"TOKEN", "SECRET", "PASSWORD", "PASS", "KEY", "CREDENTIAL"} {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}

func containsHostToken(s, host string) bool {
	host = regexp.QuoteMeta(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])` + host + `([^a-z0-9_-]|$)`)
	return re.MatchString(s)
}
