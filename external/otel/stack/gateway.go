package stack

import (
	"fmt"
	"strings"
)

// GenerateRemoteWriteYAML produces the remote_write section for prometheus.yml.
func GenerateRemoteWriteYAML(targets []RemoteWriteTarget) string {
	if len(targets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("remote_write:\n")
	for _, rw := range targets {
		b.WriteString(fmt.Sprintf("  - url: \"%s\"\n", rw.URL))
		if len(rw.Headers) > 0 {
			b.WriteString("    headers:\n")
			for k, v := range rw.Headers {
				b.WriteString(fmt.Sprintf("      %s: \"%s\"\n", k, v))
			}
		}
		if rw.BasicAuth != nil {
			b.WriteString("    basic_auth:\n")
			b.WriteString(fmt.Sprintf("      username: \"%s\"\n", rw.BasicAuth.Username))
			b.WriteString(fmt.Sprintf("      password: \"%s\"\n", rw.BasicAuth.Password))
		}
		b.WriteString("    queue_config:\n")
		b.WriteString("      max_samples_per_send: 1000\n")
		b.WriteString("      batch_send_deadline: 5s\n")
	}
	return b.String()
}

// GenerateFederationYAML produces federation scrape_config entries.
func GenerateFederationYAML(targets []FederationTarget) string {
	if len(targets) == 0 {
		return ""
	}
	var b strings.Builder
	for i, fed := range targets {
		b.WriteString(fmt.Sprintf("  - job_name: 'federate-%d'\n", i))
		b.WriteString("    honor_labels: true\n")
		b.WriteString("    metrics_path: '/federate'\n")
		b.WriteString("    params:\n      'match[]':\n")
		for _, m := range fed.Match {
			b.WriteString(fmt.Sprintf("        - '%s'\n", m))
		}
		b.WriteString(fmt.Sprintf("    static_configs:\n      - targets: ['%s']\n\n", fed.URL))
	}
	return b.String()
}
