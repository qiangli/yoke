package registry

import (
	"context"

	"github.com/qiangli/yoke/external/msgclients"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// Messaging clients (story 518107b4dfc7), tier 6. Host-only short-lived
// clients to an explicitly configured broker: none has a sha256-pinned
// managed release, so Resolve is an actionable diagnostic and nothing is
// auto-downloaded. No broker is installed or started, no persistent
// subscriber is admitted (long-lived service rules apply), and each
// broker's licence is recorded separately in external/msgclients.
func init() {
	for _, d := range []struct{ name, synopsis string }{
		{"mosquitto", "Mosquitto MQTT clients mosquitto_pub/mosquitto_sub (host-only external, BSD-3-Clause; remote broker only, no broker startup)"},
		{"rabbitmqadmin", "RabbitMQ management CLI rabbitmqadmin v2 (host-only external, MIT; remote server only)"},
		{"kcat", "Kafka client kcat (host-only external, BSD-2-Clause; remote cluster only)"},
	} {
		name := d.name
		c, _ := msgclients.Lookup(name)
		bins := ""
		if len(c.Binaries) == 1 {
			bins = c.Binaries[0]
		} else {
			bins = "mosquitto_pub and mosquitto_sub"
		}
		register(Entry{
			Name:       name,
			Tier:       6,
			License:    c.License,
			Synopsis:   d.synopsis,
			PreferHost: true,
			Long: name + ` is a short-lived client to an explicitly configured, externally managed
broker (` + c.Broker + `). bashy never installs, starts or provisions a
broker, and uses the host's own client (no managed download is admitted);
subscriptions are always bounded — persistent subscribers are long-lived
services and are not admitted here. All args pass through; the typed
bounded/publish-explicit path is msgclients.Execute.
Host binaries: ` + bins + `. Client licence: ` + c.License + `. Broker licence (separate): ` + c.BrokerLicense + `.`,
			Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
				return binmgr.Tool{}, msgclients.HostOnlyError(name)
			},
		})
	}
}
