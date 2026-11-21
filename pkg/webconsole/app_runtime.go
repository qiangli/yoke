package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/spf13/cobra"
)

type appRuntimeURLKey struct{}

func (p *serverProc) publishRuntime(st *shape.Store, r shape.Runtime) error {
	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	if p.runtimeStopped || r.PID <= 0 {
		return nil
	}
	if err := st.WriteRuntime(r); err != nil {
		return err
	}
	p.runtimeStore, p.runtimeRecord = st, r
	return nil
}

func (p *serverProc) removeRuntime() {
	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	p.runtimeStopped = true
	if p.runtimeStore != nil {
		if err := p.runtimeStore.RemoveRuntime(p.runtimeRecord); err != nil {
			slog.Warn("shaped app: cannot remove runtime", "err", err)
		}
	}
}

func addAppRuntimeFlag(c *cobra.Command) {
	var runtime bool
	c.Flags().BoolVar(&runtime, "runtime", false, "show the local operator's running shaped app instance")
	showRecord := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if !runtime {
			return showRecord(cmd, args)
		}
		if cmd.Flags().Changed("yaml") || cmd.Flags().Changed("field") {
			return errors.New("--runtime supports only --json as an output option")
		}
		name := args[0]
		if app, ok := fleet.New().App(name); ok {
			name = app.Name
		}
		r, err := localAppRuntime(cmd.Context(), name)
		if err != nil {
			return err
		}
		asJSON, err := cmd.Flags().GetBool("json")
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Address: %s\nData: %s\nURL: %s\nPID: %d\nRevision: %s\n",
			r.Addr, r.Data, r.URL, r.PID, r.Revision)
		return err
	}
}

func localAppRuntime(ctx context.Context, name string) (shape.Runtime, error) {
	principal := osPrincipal(currentOSUser())
	st, err := shape.Open(name, principal)
	if err != nil {
		return shape.Runtime{}, err
	}
	r, err := st.ReadRuntime()
	if err == nil {
		err = checkAppRuntime(ctx, st, r)
	} else if errors.Is(err, os.ErrNotExist) {
		err = errors.New("no running instance recorded")
	}
	if err != nil {
		mount := "/" + url.PathEscape(name) + "/ in the running console"
		if validRuntimeURL(r.URL) {
			mount = r.URL
		}
		env := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
		return shape.Runtime{}, fmt.Errorf("app %q: %w; open %s once, or pass %s_ADDR and %s_DATA", name, err, mount, env, env)
	}
	return r, nil
}

func validRuntimeURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Path != "" && !u.ForceQuery && u.RawQuery == "" && u.Fragment == ""
}

func checkAppRuntime(ctx context.Context, st *shape.Store, r shape.Runtime) error {
	if r.PID <= 0 || !room.PidAlive(r.PID) {
		return errors.New("runtime process has stopped")
	}
	host, port, err := net.SplitHostPort(r.Addr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || !net.ParseIP(host).IsLoopback() ||
		r.Data != filepath.Join(st.Dir(), "data") || r.Revision == "" || !validRuntimeURL(r.URL) {
		return errors.New("invalid runtime record")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.Addr+"/healthz", nil)
	if err != nil {
		return err
	}
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("runtime health check failed: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime health check returned %s", resp.Status)
	}
	return nil
}
