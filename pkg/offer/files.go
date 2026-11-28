package offer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/sharedfiles"
)

func init() { sharedfiles.RegisterOffer = RegisterFilesShare; sharedfiles.StopOffer = StopFilesShare }

// RegisterFilesShare publishes an existing F2 folder as one gated offer app.
// The files tool itself remains in broker's F1 handler.
func RegisterFilesShare(ctx context.Context, name string, circles, groups, emails []string) error {
	if len(circles)+len(groups)+len(emails) == 0 {
		return nil
	}
	if !broker.ValidPeerName(name) {
		return fmt.Errorf("invalid files offer name %q", name)
	}
	f, err := broker.FileShareDefinition(name)
	if err != nil {
		return err
	}
	var input map[string]any
	var output jsonschema.Schema
	if err := json.Unmarshal(f.InputSchema, &input); err != nil {
		return err
	}
	if err := json.Unmarshal(f.OutputSchema, &output); err != nil {
		return err
	}
	d := ToolDefinition{Name: f.Name, Class: "tool", Description: f.Description, Input: input, Output: &output, Effects: f.Effects, Unit: f.Unit}
	digest, err := d.Digest()
	if err != nil {
		return err
	}
	definition, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rec := broker.OfferRecord{Name: f.Name, Definition: definition, Digest: digest}
	rec.Audience.Circles, rec.Audience.Groups, rec.Audience.Emails = circles, groups, emails
	dir, err := broker.OffersDir()
	if err != nil {
		return err
	}
	if _, err := broker.LoadOffer(dir, f.Name); err == nil {
		return fmt.Errorf("files offer %s already exists; unshare it before changing its audience", f.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	app := "offer-" + f.Name
	if err := ensureApp(ctx, app); err != nil {
		return err
	}
	secret, err := outpostRun(ctx, "apps", "secret", app)
	if err != nil {
		return err
	}
	rec.SSOSecret = strings.TrimSpace(string(secret))
	if rec.SSOSecret == "" {
		return fmt.Errorf("outpost returned no app secret")
	}
	for _, pair := range []struct {
		flag   string
		values []string
	}{{"--circle", circles}, {"--group", groups}, {"--email", emails}} {
		for _, value := range pair.values {
			if _, err := outpostRun(ctx, "apps", "share", app, pair.flag, value); err != nil {
				return err
			}
		}
	}
	return broker.SaveOffer(dir, rec)
}

// StopFilesShare removes the public app and its offer record when present.
func StopFilesShare(ctx context.Context, name string) error {
	dir, err := broker.OffersDir()
	if err != nil {
		return err
	}
	if _, err := broker.LoadOffer(dir, "files-"+name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := outpostRun(ctx, "apps", "rm", "offer-files-"+name); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, "files-"+name+".json"))
}
