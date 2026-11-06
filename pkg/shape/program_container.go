package shape

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// containerIdentity accepts only the same narrow, trusted direct-run profile
// used by service shaping. Never substitute an embedded OCI engine for it.
func containerIdentity(u fleet.Service) (prefix []string, name, destination string, err error) {
	if _, err = eligibleProgramService(u); err != nil {
		return
	}
	start := 2
	if strings.TrimSuffix(strings.ToLower(filepath.Base(u.Exec[0])), ".exe") != "podman" {
		start = 3
	}
	prefix = append([]string(nil), u.Exec[:start-1]...)
	for i := start; i < len(u.Exec); i++ {
		switch u.Exec[i] {
		case "--name":
			i++
			if name != "" {
				err = errors.New("shape: duplicate container name")
				return
			}
			name = u.Exec[i]
		case "-v", "--volume":
			i++
			if destination != "" {
				err = errors.New("shape: multiple container mounts")
				return
			}
			destination = strings.Split(u.Exec[i], ":")[1]
		}
	}
	if name != u.Name || strings.ContainsAny(name, "/\\{} \t\n") || name == "" {
		err = errors.New("shape: container needs an explicit name equal to its trusted unit")
	}
	return
}

func (r BashyServiceRuntime) backend(ctx context.Context, prefix []string, args ...string) ([]byte, error) {
	argv := append(append([]string(nil), prefix[1:]...), args...)
	if r.run != nil {
		return r.run(ctx, prefix[0], argv...)
	}
	return exec.CommandContext(ctx, prefix[0], argv...).CombinedOutput()
}

func (r BashyServiceRuntime) stopContainer(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	u, err := r.Load(ctx, name)
	if err != nil {
		return err
	}
	prefix, container, destination, err := containerIdentity(u)
	if err != nil {
		return err
	}
	if _, err = r.command(ctx, "service", "stop", name); err != nil {
		return err
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		var doc struct {
			Units []service.Status `json:"units"`
		}
		b, e := r.command(ctx, "service", "status", name, "--json")
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &doc); e != nil {
			return fmt.Errorf("shape: invalid stopped service status: %w", e)
		}
		stopped := false
		for _, st := range doc.Units {
			if st.Unit == name && (st.State == service.Inactive || st.State == service.Failed) {
				stopped = true
			}
		}
		if stopped {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("shape: service stop incomplete: %w", ctx.Err())
		case <-tick.C:
		}
	}
	_, err = r.backend(ctx, prefix, "container", "exists", container)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("shape: cannot determine owned container existence: %w", err)
	}
	b, err := r.backend(ctx, prefix, "container", "inspect", container)
	if err != nil {
		return fmt.Errorf("shape: cannot inspect owned container: %w", err)
	}
	var docs []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Mounts []struct{ Type, Source, Destination string }
	}
	if err = json.Unmarshal(b, &docs); err != nil || len(docs) != 1 {
		return errors.New("shape: invalid owned container inspection")
	}
	d := docs[0]
	_, idErr := hex.DecodeString(d.ID)
	image := strings.TrimPrefix(d.Image, "sha256:")
	if len(d.ID) != 64 || idErr != nil || d.Name != container || "sha256:"+image != u.Settings["image"] || len(d.Mounts) != 1 || d.Mounts[0].Type != "bind" || d.Mounts[0].Source != u.Settings["data_dir"] || d.Mounts[0].Destination != destination {
		return errors.New("shape: stopped container ownership differs; keeping unit and data")
	}
	// Use the inspected full ID, so a concurrent replacement of the name is
	// never removed by this reconciliation.
	if _, err = r.backend(ctx, prefix, "container", "rm", "--force", d.ID); err != nil {
		return fmt.Errorf("shape: owned container cleanup failed: %w", err)
	}
	return nil
}
