package stack

// RemoteWriteTarget for Prometheus remote-write.
type RemoteWriteTarget struct {
	URL       string
	Headers   map[string]string
	BasicAuth *BasicAuthConfig
}

// BasicAuthConfig for remote-write authentication.
type BasicAuthConfig struct {
	Username string
	Password string
}

// FederationTarget for Prometheus federation.
type FederationTarget struct {
	URL   string
	Match []string
}
