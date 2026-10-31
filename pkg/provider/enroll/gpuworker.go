package enroll

// GPU WORKER FIRST BOOT (S546.1 slice 2 item 5). A GPU worker keeps model
// weights on the plan's model_dir (/var/lib/ollama on the instance's own gp3
// volume, which persists across EC2 Stop/Start), so custom GGUF and Ollama
// blobs are not re-downloaded after a stop. The master's first-boot script
// does not know the plan, so the user data the executable path sends is a
// short POSIX prologue that points every model server on the host at
// model_dir (OLLAMA_MODELS) and then runs the master's script UNCHANGED:
// the script that runs is byte-identical to what the master rendered.

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// gpuWorkerMarker identifies a prologue this package produced.
const gpuWorkerMarker = "# bashy-gpu-worker-prologue-v1"

// masterScriptMarker is the line after which the master's script follows.
const masterScriptMarker = "__BASHY_MASTER_SCRIPT__"

// ModelsEnv is the model server's model-store variable (bashy ollama serve
// and the broker's engine both honour it).
const ModelsEnv = "OLLAMA_MODELS"

// gpuWorkerMasterPath is where the master's script is written before exec.
const gpuWorkerMasterPath = "/var/lib/cloud/bashy-firstboot-master.sh"

// gpuWorkerModelsConf persists OLLAMA_MODELS in systemd's default
// environment, so every service the first-boot script installs sees it on
// every later boot (a Stop/Start is a reboot).
const gpuWorkerModelsConf = "/etc/systemd/system.conf.d/60-bashy-ollama-models.conf"

var safeModelDir = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

func validModelDir(dir string) error {
	if !safeModelDir.MatchString(dir) || path.Clean(dir) != dir || dir == "/" {
		return &Error{Kind: KindEnrollFailed,
			Message: fmt.Sprintf("model_dir %q must be a clean absolute path of [A-Za-z0-9._/-]", dir)}
	}
	return nil
}

// GPUWorkerUserData composes a GPU worker's first-boot user data: the
// prologue that creates modelDir and exports OLLAMA_MODELS=modelDir (for
// the running systemd manager, its default environment on later boots,
// /etc/environment and the master script itself), followed by the master's
// script byte-identical.
func GPUWorkerUserData(masterScript []byte, modelDir string) ([]byte, error) {
	if err := validModelDir(modelDir); err != nil {
		return nil, err
	}
	if len(masterScript) == 0 {
		return nil, &Error{Kind: KindBundleIncomplete,
			Message: "master issued a bundle without user_data (the first-boot script)"}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(gpuWorkerMarker + "\n")
	b.WriteString("# GPU worker first boot: keep model weights on the plan's model_dir (the\n")
	b.WriteString("# instance's own gp3 volume, persistent across Stop/Start), then run the\n")
	b.WriteString("# master's first-boot script below unchanged.\n")
	b.WriteString("set -eu\n")
	b.WriteString("model_dir='" + modelDir + "'\n")
	b.WriteString("mkdir -p \"$model_dir\"\n")
	b.WriteString("chmod 0755 \"$model_dir\"\n")
	b.WriteString("if command -v systemctl >/dev/null 2>&1; then\n")
	b.WriteString("  mkdir -p " + path.Dir(gpuWorkerModelsConf) + "\n")
	b.WriteString("  printf '[Manager]\\nDefaultEnvironment=" + ModelsEnv + "=%s\\n' \"$model_dir\" >" + gpuWorkerModelsConf + "\n")
	b.WriteString("  systemctl set-environment " + ModelsEnv + "=\"$model_dir\" || true\n")
	b.WriteString("fi\n")
	b.WriteString("grep -q '^" + ModelsEnv + "=' /etc/environment 2>/dev/null || printf '" + ModelsEnv + "=%s\\n' \"$model_dir\" >>/etc/environment\n")
	b.WriteString("export " + ModelsEnv + "=\"$model_dir\"\n")
	b.WriteString("out=" + gpuWorkerMasterPath + "\n")
	b.WriteString("mkdir -p \"$(dirname \"$out\")\"\n")
	b.WriteString("sed '1,/^" + masterScriptMarker + "$/d' \"$0\" >\"$out\"\n")
	b.WriteString("chmod 0700 \"$out\"\n")
	b.WriteString("if head -c2 \"$out\" | grep -q '#!'; then\n")
	b.WriteString("  exec \"$out\"\n")
	b.WriteString("fi\n")
	b.WriteString("exec sh \"$out\"\n")
	b.WriteString(masterScriptMarker + "\n")
	out := append([]byte(b.String()), masterScript...)
	return out, nil
}

// MasterScript returns the master's script inside GPU worker user data, and
// data unchanged when it carries no prologue.
func MasterScript(data []byte) []byte {
	if !bytes.HasPrefix(data, []byte("#!/bin/sh\n"+gpuWorkerMarker+"\n")) {
		return data
	}
	if _, script, ok := bytes.Cut(data, []byte("\n"+masterScriptMarker+"\n")); ok {
		return script
	}
	return data
}
