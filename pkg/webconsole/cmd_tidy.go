package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/src"
	"github.com/spf13/cobra"
)

var tidyShapeOptions = func(home string) ShapeOptions { return ShapeOptions{Home: home} }

func newTidyCmd() *cobra.Command {
	var user string
	var apply bool
	var keepLast, keepDays int
	c := &cobra.Command{
		Use: "tidy <app>", Short: "Preview or compact one principal's end-of-life app versions",
		Long: "Preview one user's version support window. The active, named, adopted, offered and review versions stay supported. The app may declare tidy.keep_last, tidy.keep_days and tidy.unit in shaped.json; api/tidy.md is used when present. --apply runs the declared unit with this user's private app data and compacts retired history into one restorable base. Stored-property contract changes remain a separate explicit step.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			principal := strings.TrimSpace(user)
			if principal == "" {
				principal = strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL"))
			}
			if principal == "" {
				return fmt.Errorf("app tidy needs --user or $BASHY_PRINCIPAL")
			}
			home := src.Home()
			p, err := src.PlanTidy(home, args[0], principal, src.TidyPolicy{KeepLast: keepLast, KeepDays: keepDays})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "app %s, user %s: keep last %d and last %d days\n", p.App, p.Principal, p.KeepLast, p.KeepDays)
			for _, e := range p.Squashed {
				fmt.Fprintf(out, "squash %s revision %s\n", e.Activation, e.Revision)
			}
			fmt.Fprintf(out, "%d kept, %d to squash\n", len(p.Kept), len(p.Squashed))
			if p.Unit != "" {
				fmt.Fprintf(out, "tidy unit: %s\n", p.Unit)
			}
			for _, prop := range p.ContractProperties {
				fmt.Fprintf(out, "contract step available for old property %s (requires explicit approval)\n", prop)
			}
			if !apply {
				return nil
			}
			if p.Unit != "" {
				if err := runTidyUnit(c.Context(), home, p, tidyShapeOptions(home)); err != nil {
					return err
				}
			}
			if err := src.ApplyTidy(home, p); err != nil {
				return err
			}
			fmt.Fprintln(out, "tidy applied")
			return nil
		},
	}
	c.Flags().StringVar(&user, "user", "", "principal whose versions and data are tidied (default $BASHY_PRINCIPAL)")
	c.Flags().BoolVar(&apply, "apply", false, "run the app's tidy unit and compact history")
	c.Flags().IntVar(&keepLast, "keep-last", -1, "keep this many latest versions (default app policy)")
	c.Flags().IntVar(&keepDays, "keep-days", -1, "keep versions from this many days (default app policy)")
	return c
}

// runTidyUnit uses exactly the shaped app's api unit path, including its
// per-principal boundary, effects, and private data directory.
func runTidyUnit(ctx context.Context, home string, p src.TidyPlan, opts ShapeOptions) error {
	a := &shapedApp{name: p.App, repo: filepath.Join(src.SrcDir(home), p.App), opts: opts}
	r, err := a.open()
	if err != nil {
		return err
	}
	c, err := a.active(r, p.Principal)
	if err != nil {
		return err
	}
	tr, err := c.Tree()
	if err != nil {
		return err
	}
	data, cleanup, err := a.dataDir(p.Principal)
	if err != nil {
		return err
	}
	defer cleanup()
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+p.Unit, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveAPI(w, req, tr, p.Unit, p.Principal, data)
	if w.Code != http.StatusOK {
		return fmt.Errorf("app tidy unit %s failed: %s", p.Unit, strings.TrimSpace(w.Body.String()))
	}
	return nil
}
