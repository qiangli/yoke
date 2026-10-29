// Package accounts keeps a private, host-local inventory of account metadata.
// Credential values belong in credential stores; this package accepts references only.
package accounts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = 1

type Category string

const (
	Cloudbox     Category = "cloudbox"
	Satellite    Category = "satellite"
	DigitalOcean Category = "digitalocean"
	AWS          Category = "aws"
	LLM          Category = "llm"
	HostLogin    Category = "host-login"
	Other        Category = "other"
)

type Status string

const (
	Resolved   Status = "resolved"
	Unresolved Status = "unresolved"
	Error      Status = "error"
	Suppressed Status = "suppressed"
)

type Connection struct {
	ID            string     `json:"id"`
	Endpoint      string     `json:"endpoint,omitempty"`
	Profile       string     `json:"profile,omitempty"`
	CredentialRef string     `json:"credential_ref,omitempty"`
	Scopes        []string   `json:"scopes,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	AuthMethod    string     `json:"auth_method,omitempty"`
}
type ResourceRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}
type SSHKeyRef struct {
	Fingerprint   string `json:"fingerprint"`
	Name          string `json:"name,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
}
type Links struct {
	Satellites []string `json:"satellites,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
	Models     []string `json:"models,omitempty"`
	Agents     []string `json:"agents,omitempty"`
}
type Account struct {
	ID                string        `json:"id"`
	Alias             string        `json:"alias,omitempty"`
	Category          Category      `json:"category"`
	Provider          string        `json:"provider"`
	ProviderAccountID string        `json:"provider_account_id,omitempty"`
	Name              string        `json:"name,omitempty"`
	Username          string        `json:"username,omitempty"`
	Email             string        `json:"email,omitempty"`
	AvatarURL         string        `json:"avatar_url,omitempty"`
	ProfileURL        string        `json:"profile_url,omitempty"`
	DashboardURL      string        `json:"dashboard_url,omitempty"`
	Team              string        `json:"team,omitempty"`
	Org               string        `json:"org,omitempty"`
	Project           string        `json:"project,omitempty"`
	Tenant            string        `json:"tenant,omitempty"`
	Role              string        `json:"role,omitempty"`
	Region            string        `json:"region,omitempty"`
	Environment       string        `json:"environment,omitempty"`
	Lane              string        `json:"lane,omitempty"`
	Connections       []Connection  `json:"connections,omitempty"`
	Resources         []ResourceRef `json:"resources,omitempty"`
	SSHKeys           []SSHKeyRef   `json:"ssh_keys,omitempty"`
	Links             Links         `json:"links,omitempty"`
	Status            Status        `json:"status"`
	Provenance        string        `json:"provenance,omitempty"`
	Source            string        `json:"source,omitempty"`
	ObservedAt        *time.Time    `json:"observed_at,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
}

// Observation identifies unverifiable records by SourceKey. SourceKey must be
// stable within one adapter and may not be inferred from a person's name or email.
type Observation struct {
	Account   Account `json:"account"`
	SourceKey string  `json:"source_key,omitempty"`
}

// Override fields are intentionally separate from adapter snapshots.
type Override struct {
	Alias       *string `json:"alias,omitempty"`
	Name        *string `json:"name,omitempty"`
	Region      *string `json:"region,omitempty"`
	Environment *string `json:"environment,omitempty"`
	Links       *Links  `json:"links,omitempty"`
}

var ErrNotFound = errors.New("accounts: not found")
var ErrReferenced = errors.New("accounts: account has bindings")
var ErrUnsupported = errors.New("accounts: unsupported capability")
var ErrAmbiguous = errors.New("accounts: ambiguous selection")
var ErrNewerSchema = errors.New("accounts: newer schema version")

var refPattern = regexp.MustCompile(`^(secret:[A-Za-z0-9][A-Za-z0-9_.-]*|ssh-key:[A-Za-z0-9][A-Za-z0-9:_.-]*|env:[A-Za-z_][A-Za-z0-9_]*|profile:[A-Za-z0-9][A-Za-z0-9_.-]*)$`)
var errorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func ValidCredentialRef(ref string) bool { return refPattern.MatchString(ref) }
func validateAccount(a Account) error {
	if a.Category == "" || a.Provider == "" {
		return errors.New("accounts: category and provider required")
	}
	switch a.Category {
	case Cloudbox, Satellite, DigitalOcean, AWS, LLM, HostLogin, Other:
	default:
		return errors.New("accounts: invalid category")
	}
	for _, c := range a.Connections {
		if c.CredentialRef != "" && !ValidCredentialRef(c.CredentialRef) {
			return errors.New("accounts: invalid credential reference")
		}
	}
	for _, k := range a.SSHKeys {
		if k.CredentialRef != "" && !ValidCredentialRef(k.CredentialRef) {
			return errors.New("accounts: invalid credential reference")
		}
	}
	if a.Status != "" && a.Status != Resolved && a.Status != Unresolved && a.Status != Error && a.Status != Suppressed {
		return errors.New("accounts: invalid status")
	}
	// Persist an error code, never a provider error message (which may include credentials).
	if a.LastError != "" && !errorCodePattern.MatchString(a.LastError) {
		return errors.New("accounts: last_error must be a safe code")
	}
	if strings.ContainsAny(a.Alias, "\n\r\t") {
		return fmt.Errorf("accounts: invalid alias")
	}
	return nil
}
