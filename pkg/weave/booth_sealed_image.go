package weave

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/external/podman"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/spf13/cobra"
)

type boothSealedImagePlan struct {
	Tag           string
	Containerfile string
	Args          []string
}

var boothSealedImageComponent = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var boothSealedImageVersion = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func boothSealedImageTag(t fleet.Tool, d *fleet.CommandDownload, override string) (string, error) {
	if override != "" {
		if strings.HasPrefix(override, "-") || strings.ContainsAny(override, " \t\r\n") {
			return "", fmt.Errorf("invalid sealed image tag %q", override)
		}
		return override, nil
	}
	version := ""
	if d != nil {
		version = d.Version
	} else if len(t.CLI.Versions) > 0 {
		version = t.CLI.Versions[0].Version
	}
	if !boothSealedImageComponent.MatchString(t.Name) {
		return "", fmt.Errorf("sealed image: invalid tool name %q", t.Name)
	}
	if !boothSealedImageVersion.MatchString(version) {
		return "", fmt.Errorf("sealed image for %s: missing or invalid cli.versions.version (or download.version)", t.Name)
	}
	return "localhost/bashy-sealed-agent-" + t.Name + ":" + version, nil
}

func boothSealedImageQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Only a declared npm recipe or a pinned Linux command download is accepted.
// Host installation commands are never executed or copied into a Linux image.
func boothSealedPlanImage(t fleet.Tool, d *fleet.CommandDownload, tag, arch string) (boothSealedImagePlan, error) {
	var p boothSealedImagePlan
	if arch != "amd64" && arch != "arm64" {
		return p, fmt.Errorf("sealed image: unsupported Linux architecture %q", arch)
	}
	binary := filepath.Base(t.CLI.Binary)
	if t.CLI.Linux.Binary != "" {
		binary = filepath.Base(t.CLI.Linux.Binary)
	}
	if !boothSealedImageComponent.MatchString(binary) {
		return p, fmt.Errorf("sealed image for %s: missing or invalid cli.binary", t.Name)
	}
	var install string
	packages := "ca-certificates git"
	if t.CLI.Linux.Install != "" {
		if t.CLI.Linux.Binary == "" {
			return p, fmt.Errorf("sealed image for %s: missing cli.linux.binary", t.Name)
		}
		if len(t.CLI.Versions) == 0 || !boothSealedImageVersion.MatchString(t.CLI.Versions[0].Version) {
			return p, fmt.Errorf("sealed image for %s: missing or invalid cli.versions.version for cli.linux", t.Name)
		}
		if strings.ContainsAny(t.CLI.Linux.Install, "\r\n") {
			return p, fmt.Errorf("sealed image for %s: invalid cli.linux.install", t.Name)
		}
		for _, required := range t.CLI.Linux.Requires {
			if !boothSealedImageComponent.MatchString(required) {
				return p, fmt.Errorf("sealed image for %s: invalid cli.linux.requires package %q", t.Name, required)
			}
			packages += " " + required
		}
		install = strings.ReplaceAll(t.CLI.Linux.Install, "{version}", t.CLI.Versions[0].Version)
	} else if d != nil {
		if d.Version == "" {
			return p, fmt.Errorf("sealed image for %s: missing download.version", t.Name)
		}
		if d.URL == "" {
			return p, fmt.Errorf("sealed image for %s: missing download.url Linux template (GitHub API discovery is not supported offline)", t.Name)
		}
		sha := d.SHA256["linux/"+arch]
		if matched, _ := regexp.MatchString(`^[a-fA-F0-9]{64}$`, sha); !matched {
			return p, fmt.Errorf("sealed image for %s: missing or invalid download.sha256.linux/%s", t.Name, arch)
		}
		source := strings.NewReplacer("{version}", d.Version, "{goos}", "linux", "{goarch}", arch, "{ext}", "").Replace(d.URL)
		u, err := url.Parse(source)
		if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(source, "\r\n{}") {
			return p, fmt.Errorf("sealed image for %s: invalid download.url", t.Name)
		}
		packages += " curl xz-utils unzip zstd"
		install = "curl --fail --location --proto '=https' --proto-redir '=https' --output /tmp/agent-download " + boothSealedImageQuote(source) + " && printf '%s  /tmp/agent-download\\n' " + boothSealedImageQuote(sha) + " | sha256sum -c -"
		dest := boothSealedImageQuote("/usr/local/bin/" + binary)
		if d.Member == "" {
			install += " && cp /tmp/agent-download " + dest
		} else {
			if path.IsAbs(d.Member) || path.Clean(d.Member) != d.Member || strings.HasPrefix(d.Member, "-") || strings.HasPrefix(d.Member, "../") || strings.ContainsAny(d.Member, "\r\n") {
				return p, fmt.Errorf("sealed image for %s: invalid download.member", t.Name)
			}
			member := boothSealedImageQuote(d.Member)
			switch {
			case strings.HasSuffix(u.Path, ".zip"):
				install += " && unzip -p /tmp/agent-download " + member + " > " + dest
			case strings.HasSuffix(u.Path, ".tar.gz"), strings.HasSuffix(u.Path, ".tar.xz"), strings.HasSuffix(u.Path, ".tar.zst"):
				install += " && tar -xOf /tmp/agent-download -- " + member + " > " + dest
			default:
				return p, fmt.Errorf("sealed image for %s: unsupported download.url archive format", t.Name)
			}
		}
		install += " && chmod 0755 " + dest + " && rm /tmp/agent-download"
	} else {
		return p, fmt.Errorf("sealed image for %s: missing Linux recipe cli.linux.install or command download.url", t.Name)
	}
	resolved, err := boothSealedImageTag(t, d, tag)
	if err != nil {
		return p, err
	}
	p.Tag = resolved
	p.Containerfile = "FROM docker.io/library/debian:bookworm-slim\nRUN apt-get update && apt-get install -y --no-install-recommends " + packages + " && rm -rf /var/lib/apt/lists/*\nRUN " + install + "\nRUN mkdir -p /home/booth /workspace\nENV HOME=/home/booth\nWORKDIR /workspace\n"
	// The complete build context is a tar on stdin containing only Containerfile.
	p.Args = []string{"build", "--platform", "linux/" + arch, "--tag", p.Tag, "--file", "Containerfile", "-"}
	return p, nil
}

func boothSealedImageContext(p boothSealedImagePlan) (string, error) {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "Containerfile", Mode: 0600, Size: int64(len(p.Containerfile))}); err != nil {
		return "", err
	}
	if _, err := w.Write([]byte(p.Containerfile)); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// The standard seeder retains host HOME/config paths for keychain identities.
// A sealed login must instead live wholly in the private booth directories.
func boothSealedCheckLogin(tool, dir string, run int64, env []string) error {
	home := filepath.Join(dir, fmt.Sprintf("booth-%d-home", run))
	agent := filepath.Join(dir, fmt.Sprintf("booth-%d-agent", run))
	for _, key := range []string{"HOME", boothConfigEnv(tool)} {
		value := boothGetEnv(env, key)
		if value == "" && key != "HOME" {
			continue
		}
		if value != home && !strings.HasPrefix(value, home+string(filepath.Separator)) && value != agent && !strings.HasPrefix(value, agent+string(filepath.Separator)) {
			return fmt.Errorf("sealed booth unsupported for %s: login is not file-based on this host", tool)
		}
	}
	return nil
}

func newWeaveSealedImageCmd() *cobra.Command {
	group := &cobra.Command{Use: "sealed-image", Short: "Build Linux agent images for sealed booths"}
	var name, tag string
	var dry bool
	build := &cobra.Command{Use: "build --tool TOOL [--tag TAG] [--dry-run]", Short: "Build from the tool registry's Linux installation recipe", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, ok := fleetCatalog().Tool(name)
			if !ok {
				return fmt.Errorf("unknown tool %q", name)
			}
			c, _ := fleetCatalog().Command(filepath.Base(t.CLI.Binary))
			plan, err := boothSealedPlanImage(t, c.Download, tag, runtime.GOARCH)
			if err != nil {
				return err
			}
			if dry {
				argv, _ := json.Marshal(append([]string{"podman"}, plan.Args...))
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Containerfile:\n%s\npodman argv: %s\nstdin: tar build context containing only Containerfile above\n", plan.Containerfile, argv)
				return err
			}
			stdin, err := boothSealedImageContext(plan)
			if err != nil {
				return err
			}
			bin, err := podman.Resolve()
			if err != nil {
				return err
			}
			out, err := boothSealedPodman(cmd.Context(), bin, os.Environ(), stdin, plan.Args...)
			if out != "" {
				fmt.Fprintln(cmd.OutOrStdout(), out)
			}
			return err
		}}
	build.Flags().StringVar(&name, "tool", "", "Tool registry name")
	build.Flags().StringVar(&tag, "tag", "", "Image tag (default: tool name and registered version)")
	build.Flags().BoolVar(&dry, "dry-run", false, "Print the Containerfile and exact podman argv without podman or downloads")
	_ = build.MarkFlagRequired("tool")
	group.AddCommand(build)
	return group
}
