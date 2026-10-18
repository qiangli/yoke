// Package msgclients admits three messaging clients — mosquitto
// (mosquitto_pub/mosquitto_sub), rabbitmqadmin (v2) and kcat — as
// short-lived clients to an explicitly configured, externally managed
// broker (Sprint 457 story 518107b4dfc7, client admission contract mode
// "external user-managed endpoint").
//
// What this is not: no broker is installed, started, restarted or
// provisioned, and no listener or resident child exists. A subscription
// here is always bounded (finite message/byte/time limits with defaults),
// so a persistent subscriber — a long-lived service under the service
// supply-chain policy (Rule 1) — is inexpressible, not merely
// discouraged. A client licence is recorded here; the licence of whatever
// broker answers at the endpoint is a separate fact (Broker /
// BrokerLicense) and is never inherited from, or granted by, the client.
//
// Provisioning is host-first (registry PreferHost): none of the three
// ships a pinned, hash-verified standalone release in pkg/binmgr/pins.go,
// so an absent host client is an actionable diagnostic, never an
// auto-download.
//
// The typed operation (Build/Run) is the guarded path: explicit broker,
// TLS verify by default (plaintext only to loopback), credentials only
// via env/file (never argv), payloads on stdin or a 0600 file (never
// argv), bounded reads, context cancellation, and an explicit publish
// effect (publish reports "published"; subscribe reports "none"). The
// `bashy <client>` registry front-door stays a raw passthrough, like gws.
// Errors reuse external/gws typed codes over the loom-v2 envelope; no
// competing error ABI or schema is defined here.
package msgclients

import (
	"fmt"
	"sort"
)

// Client is the admission record of one messaging client.
type Client struct {
	Name          string   // verb
	Binaries      []string // host binary basenames (mosquitto ships pub and sub)
	Prefix        string   // env prefix for explicit endpoint/credential selection
	License       string   // SPDX id of the CLIENT only (selected dual-licence option where applicable)
	Broker        string   // what usually answers at the endpoint
	BrokerLicense string   // licence of that broker/service — separate from the client
	PortTLS       int      // default port when TLS is verify
	PortPlain     int      // default port when TLS is plaintext-loopback
	Fidelity      string   // declared output representation (lossy ones say so)
	InstallHint   string
}

var clients = map[string]Client{
	"mosquitto": {
		Name:     "mosquitto",
		Binaries: []string{"mosquitto_pub", "mosquitto_sub"},
		Prefix:   "MOSQUITTO", License: "BSD-3-Clause",
		PortTLS: 8883, PortPlain: 1883,
		Broker:        "Eclipse Mosquitto broker, or any MQTT v3.1/v3.1.1/v5 broker",
		BrokerLicense: "EPL-2.0 OR EDL-1.0 for the Eclipse Mosquitto broker; other brokers and managed services carry their own terms — recorded separately, not admitted by this client",
		Fidelity:      "framed text (unit/record separators): topic and payload strings verbatim; invalid UTF-8 is replaced on JSON encoding (declared lossy)",
		InstallHint:   "install the Mosquitto clients (mosquitto-clients) and put mosquitto_pub and mosquitto_sub on PATH",
	},
	"rabbitmqadmin": {
		Name:     "rabbitmqadmin",
		Binaries: []string{"rabbitmqadmin"},
		Prefix:   "RABBITMQ", License: "MIT",
		PortTLS: 15671, PortPlain: 15672,
		Broker:        "RabbitMQ server with the management plugin (or a compatible endpoint)",
		BrokerLicense: "MPL-2.0 for the RabbitMQ server — recorded separately, not admitted by this client",
		Fidelity:      "provider JSON per message: routing key and payload strings verbatim; numbers preserved verbatim (json.Number)",
		InstallHint:   "install rabbitmqadmin v2 and put rabbitmqadmin on PATH",
	},
	"kcat": {
		Name:     "kcat",
		Binaries: []string{"kcat"},
		Prefix:   "KAFKA", License: "BSD-2-Clause",
		PortTLS: 9093, PortPlain: 9092,
		Broker:        "Apache Kafka cluster (or a wire-compatible endpoint)",
		BrokerLicense: "Apache-2.0 for Apache Kafka — recorded separately, not admitted by this client",
		Fidelity:      "kcat -J JSON per message: topic/partition/offset numeric, payload string verbatim; invalid UTF-8 is replaced on JSON encoding (declared lossy)",
		InstallHint:   "install kcat (bundling librdkafka) and put kcat on PATH",
	},
}

// Lookup returns the admission record for a client name.
func Lookup(name string) (Client, bool) { c, ok := clients[name]; return c, ok }

// Names lists the admitted client names, sorted.
func Names() []string {
	out := make([]string, 0, len(clients))
	for n := range clients {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HostOnlyError is the registry Resolve result: no managed download is
// admitted, so an absent host client yields an actionable diagnostic.
func HostOnlyError(name string) error {
	c, ok := Lookup(name)
	if !ok {
		return fmt.Errorf("%s: not an admitted messaging client", name)
	}
	return fmt.Errorf("%s not found on PATH and no sha256-pinned managed release is admitted — %s", name, c.InstallHint)
}
