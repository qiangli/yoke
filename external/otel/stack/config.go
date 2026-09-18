package stack

const DefaultProxyPort = 31415

// Config controls the embedded OTEL stack. Only the proxy and OTLP ingress
// ports are public; component UI/backend ports are allocated on loopback.
type Config struct {
	ProxyPort     int
	ProxyBindAddr string
	OTLPGRPCPort  int
	OTLPHTTPPort  int

	RemoteWrite []RemoteWriteTarget
	Federation  []FederationTarget
}

func (c *Config) proxyPort() int {
	if c != nil && c.ProxyPort != 0 {
		return c.ProxyPort
	}
	return DefaultProxyPort
}

func (c *Config) proxyBindAddr() string {
	if c != nil && c.ProxyBindAddr != "" {
		return c.ProxyBindAddr
	}
	return "127.0.0.1"
}
