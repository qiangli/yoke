#!/usr/bin/env python3
"""Story 2018 source-only gate; no native compilation, generation, or execution.

Requires the reviewed commits in external/ollama/src's object database, network
access to official GitHub metadata, Go, and cached/downloadable Go modules.
Prints JSON evidence to stdout. This is NOT a release/build/runtime gate.
"""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "external/ollama/src"
PIN = "590109c8352e8d5a6206e8909b518a54a2b0a7b8"
FORK = "43768325f"  # Expanded from the object database in evidence.
BASE = "4656a07e5639a1edb1aaae5ea2c7dcb4021846f3"
TAG = "v0.21.2"


def run(args, **kwargs):
    return subprocess.check_output(args, **kwargs)


def git(*args):
    return run(["git", "-C", str(SRC), *args]).decode()


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def section(text, start, end):
    return text[text.index(start):text.index(end, text.index(start))]


def objects(text):
    decoder = json.JSONDecoder()
    while text.strip():
        obj, count = decoder.raw_decode(text.lstrip())
        yield obj
        text = text.lstrip()[count:]


def main():
    release_url = "https://api.github.com/repos/ollama/ollama/releases/tags/" + TAG
    with urllib.request.urlopen(release_url, timeout=30) as response:
        release = json.load(response)
    require(release["tag_name"] == TAG and not release["draft"] and
            not release["prerelease"], "not an official stable release")
    refs = run(["git", "ls-remote", "--tags", "https://github.com/ollama/ollama.git",
                "refs/tags/" + TAG, "refs/tags/" + TAG + "^{}"]).decode()
    refmap = dict(line.split()[::-1] for line in refs.splitlines())
    require(refmap.get("refs/tags/" + TAG + "^{}",
                       refmap.get("refs/tags/" + TAG)) == PIN, "official pin moved")
    git("merge-base", "--is-ancestor", PIN, BASE)
    git("merge-base", "--is-ancestor", BASE, FORK)
    require(not git("diff", "--name-only", PIN, FORK, "--", "cmd/runner",
                    "runner", "ml"), "runner/backend protocol source changed")
    old = git("show", PIN + ":llm/server.go")
    current = git("show", FORK + ":llm/server.go")
    spans = [
        ("func StartRunner(", "func (s *llmServer) ModelPath("),
        ("type LoadOperation int", "var ErrLoadRequiredFull"),
        ("type ServerStatus int", "func (s *llmServer) getServerStatus("),
    ]
    for start, end in spans:
        require(section(old, start, end) == section(current, start, end),
                "launch/load/health protocol changed: " + start)
    result = {
        "verdict": "PASS_SOURCE_ONLY_NOT_RELEASE_ADMISSION",
        "tag": TAG, "commit": PIN, "fork_commit": git("rev-parse", FORK).strip(),
        "upstream_base": BASE, "release_url": release["html_url"],
        "published_at": release["published_at"], "draft": False, "prerelease": False,
        "commits_tag_to_base": int(git("rev-list", "--count", PIN + ".." + BASE)),
        "identical_trees": ["cmd/runner", "runner", "ml"],
        "identical_protocol_sections": [start for start, _ in spans],
        "platforms": {}, "modules": {},
        "native_build": "NOT_RUN", "runtime_compatibility": "NOT_RUN",
        "vulnerability_audit": "NOT_RUN", "compiled_input_license_audit": "NOT_RUN",
    }
    with tempfile.TemporaryDirectory(prefix="ollama-source-review-") as tmp:
        archive = run(["git", "-C", str(SRC), "archive", PIN])
        with tarfile.open(fileobj=io.BytesIO(archive)) as tf:
            tf.extractall(tmp, filter="data")
        for target in ("linux/amd64", "linux/arm64", "darwin/amd64",
                       "darwin/arm64", "windows/amd64"):
            goos, goarch = target.split("/")
            env = dict(os.environ, GOWORK="off", GOOS=goos, GOARCH=goarch,
                       CGO_ENABLED="1", GOFLAGS="")
            packages = list(objects(run(["go", "list", "-mod=readonly", "-deps",
                                         "-json", "./cmd/runner"],
                                        cwd=tmp, env=env).decode()))
            require(packages and all(not any(p.get(k) for k in
                    ("Error", "DepsErrors", "Incomplete")) for p in packages),
                    "incomplete dependency graph: " + target)
            result["platforms"][target] = {
                "packages": len(packages),
                "cgo_packages": sum(bool(p.get("CgoFiles")) for p in packages),
                "dependency_resolution": "PASS",
            }
            for p in packages:
                module = p.get("Module")
                if not module or module.get("Main"):
                    continue
                directory = Path(module["Dir"])
                licenses = sorted(f for f in directory.iterdir()
                                  if f.name.lower().startswith(("license", "notice")))
                require(licenses, "missing module license: " + module["Path"])
                result["modules"][module["Path"]] = {
                    "version": module["Version"],
                    "license_files_sha256": {
                        f.name: hashlib.sha256(f.read_bytes()).hexdigest() for f in licenses
                    },
                }
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
