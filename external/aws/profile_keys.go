package aws

import (
	"bufio"
	"errors"
	"os"
	"sort"
	"strings"

	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
)

// ResolveProfileKeys reads only the explicitly named shared-file profile, or
// the explicit environment selection already validated by ResolveCredentials.
// It never invokes credential_process, assumes a role, or falls back to default.
// Role/SSO profiles require a caller-supplied session credential resolver.
func ResolveProfileKeys(profile string) (awsprovider.Credentials, error) {
	fail := errors.New("aws: explicit profile credentials unavailable")
	if profile == "" || strings.ContainsAny(profile, "\r\n[]") {
		return awsprovider.Credentials{}, fail
	}
	if os.Getenv(EnvAccessKeyID) != "" || os.Getenv(EnvSecretAccessKey) != "" {
		selected, e := ResolveCredentials()
		if e != nil || selected.Profile != profile || selected.Kind != AuthEnvKeys {
			return awsprovider.Credentials{}, fail
		}
		keys, credentialErr := awsprovider.CredentialsFromEnv(os.Environ())
		if credentialErr != nil {
			return awsprovider.Credentials{}, fail
		}
		return keys, nil
	}
	path := strings.TrimSpace(os.Getenv(EnvSharedCredentials))
	if path == "" {
		path = defaultSharedFile()
	}
	f, e := os.Open(path)
	if e != nil {
		return awsprovider.Credentials{}, fail
	}
	defer f.Close()
	section := ""
	seen := false
	values := map[string]string{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == profile {
				if seen {
					return awsprovider.Credentials{}, fail
				}
				seen = true
			}
			continue
		}
		if section != profile {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return awsprovider.Credentials{}, fail
		}
		k = strings.TrimSpace(k)
		if _, exists := values[k]; exists {
			return awsprovider.Credentials{}, fail
		}
		values[k] = strings.TrimSpace(v)
	}
	if scan.Err() != nil {
		return awsprovider.Credentials{}, fail
	}
	for _, k := range []string{"role_arn", "source_profile", "credential_process", "sso_session"} {
		if values[k] != "" {
			return awsprovider.Credentials{}, fail
		}
	}
	keys := awsprovider.Credentials{AccessKeyID: values["aws_access_key_id"], SecretAccessKey: values["aws_secret_access_key"], SessionToken: values["aws_session_token"]}
	if !keys.Valid() {
		return awsprovider.Credentials{}, fail
	}
	return keys, nil
}

// ConfiguredProfiles lists shared-credentials section names without exposing
// their values. It is configuration discovery, not an authentication check.
func ConfiguredProfiles() ([]string, error) {
	path := strings.TrimSpace(os.Getenv(EnvSharedCredentials))
	if path == "" {
		path = defaultSharedFile()
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("aws: profile configuration unavailable")
	}
	defer f.Close()
	profiles := map[string]bool{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name != "" {
				profiles[name] = true
			}
		}
	}
	if scan.Err() != nil {
		return nil, errors.New("aws: profile configuration unavailable")
	}
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
