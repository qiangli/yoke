package dataclients

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
// secret material: it is carried to the child by environment only and is
// never part of argv, RedactedScope, or any diagnostic.
type Endpoint struct {
	Client   string
	Host     string
	Port     int
	Database string
	User     string
	TLS      TLSMode
	CAFile   string
	Password string
}

// ResolveEndpoint builds an Endpoint from explicit <PREFIX>_* environment:
// HOST (required), PORT, DATABASE, USER, TLS, CA_FILE, and the password as
// PASSWORD or PASSWORD_FILE (as materialised by `bashy secret`; never both).
// There is no ambient fallback (no PGHOST, REDISCLI_*, default localhost).
func ResolveEndpoint(client string, getenv func(string) string) (Endpoint, *gws.Error) {
	c, ok := Lookup(client)
	if !ok {
		return Endpoint{}, gws.NewError(gws.CodeUnsupported, client+": not an admitted data client", nil)
	}
	get := func(k string) string { return strings.TrimSpace(getenv(c.Prefix + "_" + k)) }
	ep := Endpoint{Client: client, Host: get("HOST"), Database: get("DATABASE"), User: get("USER"), CAFile: get("CA_FILE"), Port: c.DefaultPort}
	if ep.Host == "" {
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no endpoint selected — set %s_HOST explicitly; there is no ambient or default-localhost fallback", client, c.Prefix), nil)
	}
	if p := get("PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s_PORT %q is not a port", client, c.Prefix, p), nil)
		}
		ep.Port = n
	}
	ep.TLS = TLSMode(get("TLS"))
	if ep.TLS == "" {
		ep.TLS = TLSVerify
	}
	switch ep.TLS {
	case TLSVerify:
	case TLSPlaintextLoopback:
		if !isLoopback(ep.Host) {
			return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: plaintext is allowed only to a loopback host, not %q", client, ep.Host), nil)
		}
	default:
		return Endpoint{}, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: %s_TLS %q unknown (want %s or %s)", client, c.Prefix, ep.TLS, TLSVerify, TLSPlaintextLoopback), nil)
	}
	if client == "snow" && ep.TLS != TLSVerify {
		return Endpoint{}, gws.NewError(gws.CodeInvalidInput, "snow: Snowflake is HTTPS-only; plaintext is not a valid mode", nil)
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
	if pw == "" {
		return Endpoint{}, gws.NewError(gws.CodeMissingCredentials, fmt.Sprintf("%s: no credential selected — set %s_PASSWORD or %s_PASSWORD_FILE", client, c.Prefix, c.Prefix), nil)
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
	return fmt.Sprintf("client=%s endpoint=%s:%d database=%s user=%s tls=%s credential=<set>", e.Client, e.Host, e.Port, e.Database, e.User, e.TLS)
}
