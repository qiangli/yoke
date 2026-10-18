package msgclients

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/external/gws"
)

// TLSMode is the transport policy for an endpoint.
type TLSMode string

const (
	// TLSVerify requires TLS with certificate and hostname verification.
	TLSVerify TLSMode = "verify"
	// TLSPlaintextLoopback permits plaintext, and only to a loopback host.
	TLSPlaintextLoopback TLSMode = "plaintext-loopback"
)

// Endpoint is a fully explicit target + credential selection. Password is
// secret material: it is carried to the child by environment only (kcat
// additionally receives it inside a 0600 config file written at Run; the
// mosquitto password travels as the first stdin line, consumed as the
// interactive password prompt) and is never part of argv,
// RedactedScope, or any diagnostic.
type Endpoint struct {
	Client   string
	Host     string
	Port     int
	User     string
	TLS      TLSMode
	CAFile   string
	Password string // "" means anonymous where the client allows it
}

// ResolveEndpoint builds an Endpoint from explicit <PREFIX>_* environment:
// HOST (required), PORT, USER, TLS, CA_FILE, and the password as PASSWORD
// or PASSWORD_FILE (as materialised by `bashy secret`; never both). There
// is no ambient fallback (no MQTT_*, RABBITMQ_*, KAFKA_* inheritance).
//
// Credential policy per client: mosquitto and kcat allow anonymous access
// (no USER, no PASSWORD); a USER without a PASSWORD is refused because
// the non-interactive operation cannot prompt. rabbitmqadmin fronts the
// management API, which has no anonymous mode, so USER and PASSWORD are
// both required. A PASSWORD without a USER is refused everywhere.
func ResolveEndpoint(client string, getenv func(string) string) (Endpoint, *gws.Error) {
	c, ok := Lookup(client)
	if !ok {
		return Endpoint{}, gws.NewError(gws.CodeUnsupported, client+": not an admitted messaging client", nil)
	}
	get := func(k string) string { return strings.TrimSpace(getenv(c.Prefix + "_" + k)) }
	ep := Endpoint{Client: client, Host: get("HOST"), User: get("USER"), CAFile: get("CA_FILE")}
	if ep.Host == "" {
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no endpoint selected — set %s_HOST explicitly; there is no ambient or default-localhost fallback", client, c.Prefix), nil)
	}
	ep.TLS = TLSMode(get("TLS"))
	if ep.TLS == "" {
		ep.TLS = TLSVerify
	}
	switch ep.TLS {
	case TLSVerify:
		ep.Port = c.PortTLS
	case TLSPlaintextLoopback:
		if !isLoopback(ep.Host) {
			return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: plaintext is allowed only to a loopback host, not %q", client, ep.Host), nil)
		}
		ep.Port = c.PortPlain
	default:
		return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s_TLS %q unknown (want %s or %s)", client, c.Prefix, ep.TLS, TLSVerify, TLSPlaintextLoopback), nil)
	}
	if p := get("PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s_PORT %q is not a port", client, c.Prefix, p), nil)
		}
		ep.Port = n
	}
	pw, pwFile := get("PASSWORD"), get("PASSWORD_FILE")
	switch {
	case pw != "" && pwFile != "":
		return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: ambiguous credentials — set %s_PASSWORD or %s_PASSWORD_FILE, not both", client, c.Prefix, c.Prefix), nil)
	case pwFile != "":
		b, err := os.ReadFile(pwFile)
		if err != nil {
			return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: password file unreadable (%v)", client, redactPathErr(err)), nil)
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	switch {
	case client == "rabbitmqadmin" && ep.User == "":
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no credential selected — the management API has no anonymous mode; set %s_USER and %s_PASSWORD (or %s_PASSWORD_FILE)", client, c.Prefix, c.Prefix, c.Prefix), nil)
	case client == "rabbitmqadmin" && pw == "":
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no credential selected — set %s_PASSWORD or %s_PASSWORD_FILE", client, c.Prefix, c.Prefix), nil)
	case ep.User == "" && pw != "":
		return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s_PASSWORD without %s_USER is refused — anonymous access carries no password", client, c.Prefix, c.Prefix), nil)
	case ep.User != "" && pw == "":
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: %s_USER is set but no password was selected — set %s_PASSWORD or %s_PASSWORD_FILE (this operation cannot prompt)", client, c.Prefix, c.Prefix, c.Prefix), nil)
	}
	ep.Password = pw
	return ep, nil
}

func redactPathErr(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return "read failed"
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// RedactedScope reports the resolved target with no secret material.
func (e Endpoint) RedactedScope() string {
	auth := "anonymous"
	if e.Password != "" {
		auth = "set"
	}
	return fmt.Sprintf("client=%s endpoint=%s:%d user=%s tls=%s credential=<%s>", e.Client, e.Host, e.Port, e.User, e.TLS, auth)
}
