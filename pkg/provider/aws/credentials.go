// Credential resolution from the environment (S522.2 item 2).
//
// The keys arrive projected into the process environment — in practice by
// `bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- ...`, which
// resolves them from the vault and never writes them to disk. This package
// only READS that environment; it never resolves a vault reference itself,
// and it never writes a key anywhere (no log line, no error message, no
// journal record). The one place a key id appears is the Authorization
// header AWS requires, and the one place the secret appears is inside the
// HMAC chain in sigv4.go.
//
// external/aws (the `bashy aws` front door, main module) enforces the
// stricter `AWS_PROFILE` + single-credential-source rule for the aws CLI.
// pkg/provider is a nested stdlib-only module and cannot import it, so the
// two share the env NAMES (identical constants, pinned by a test in
// external/aws) rather than code.
package aws

import (
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// Env names, identical to external/aws's.
const (
	EnvAccessKeyID     = "AWS_ACCESS_KEY_ID"
	EnvSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	EnvSessionToken    = "AWS_SESSION_TOKEN"
	EnvRegion          = "AWS_REGION"
	EnvDefaultRegion   = "AWS_DEFAULT_REGION"
)

// CredentialsFromEnv reads the key pair (and an optional session token)
// from env, which is os.Environ()'s shape ("NAME=value"). It reports a
// normalized invalid_credentials error naming what is missing, never the
// value of anything it did find.
func CredentialsFromEnv(env []string) (Credentials, *provider.Error) {
	get := func(name string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(env[i], name+"="); ok {
				return v
			}
		}
		return ""
	}
	c := Credentials{
		AccessKeyID:     strings.TrimSpace(get(EnvAccessKeyID)),
		SecretAccessKey: get(EnvSecretAccessKey),
		SessionToken:    strings.TrimSpace(get(EnvSessionToken)),
	}
	var missing []string
	if c.AccessKeyID == "" {
		missing = append(missing, EnvAccessKeyID)
	}
	if c.SecretAccessKey == "" {
		missing = append(missing, EnvSecretAccessKey)
	}
	if len(missing) > 0 {
		return Credentials{}, &provider.Error{Code: provider.ErrInvalidCredentials, Message: "aws: no credentials in the environment: " +
			strings.Join(missing, " and ") + " not set (project them with `bashy secret exec " +
			EnvAccessKeyID + " " + EnvSecretAccessKey + " -- ...`)"}
	}
	return c, nil
}

// RegionFromEnv returns the region named in env, or "" when none is.
func RegionFromEnv(env []string) string {
	for _, name := range []string{EnvRegion, EnvDefaultRegion} {
		for i := len(env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(env[i], name+"="); ok {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}
