package registry

import (
	"context"
	"os"

	"github.com/qiangli/yoke/external/saasclients"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Salesforce, Stripe, Twilio and Slack CLIs (story c512b342864d), tier 6,
// short-lived clients only. Host-first like gws/gcloud: no sha256 pins land in
// pkg/binmgr/pins.go yet, so an unset version never TOFU-downloads. stripe and
// slack may opt into a managed GitHub-release download via $STRIPE_VERSION /
// $SLACK_VERSION (verified against the release asset digest); sf and twilio
// are host-only (Node/oclif distributions, no admitted archive). Webhook
// forwarding/serving and other resident modes are NOT admitted — see
// saasclients.ClassifyArgs/AdmitMode.
func init() {
	for _, d := range []struct{ name, env, synopsis string }{
		{"sf", "", "Salesforce CLI sf (host-only external, Apache-2.0; explicit sandbox org only)"},
		{"stripe", "STRIPE_VERSION", "Stripe CLI (managed external, Apache-2.0; host-first, test-mode only)"},
		{"twilio", "", "Twilio CLI (host-only external, MIT; sandbox account only)"},
		{"slack", "SLACK_VERSION", "Slack CLI (managed external, Apache-2.0; app tooling — messaging via Web API adapter)"},
	} {
		name := d.name
		c, _ := saasclients.Lookup(name)
		register(Entry{
			Name:       name,
			Tier:       6,
			License:    c.License,
			Synopsis:   d.synopsis,
			EnvVersion: d.env,
			PreferHost: true,
			Long: name + ` (` + c.Repo + `, ` + c.License + `; reviewed ` + c.Version + `) is a short-lived client with
explicit target and credential selection (no ambient account). Webhook
forwarding/serving, tails, sockets and callback-listener logins are not admitted.
All args pass through. Remote service (separate): ` + c.RemoteService + `.`,
			ExecEnv: func() []string { return saasclients.ChildEnv(name, os.Environ()) },
			Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
				spec, err := saasclients.Spec(name, version)
				if err != nil {
					return binmgr.Tool{}, err
				}
				return binmgr.ResolveGitHub(ctx, spec)
			},
		})
	}
}
