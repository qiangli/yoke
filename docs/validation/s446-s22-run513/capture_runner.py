#!/usr/bin/python3
"""Evidence-only native runner adapter; config lives beside the deployed copy.

Captures the actual lens-script pipe bytes before forwarding them to weave's
PTY. Never executes task text itself. No model calls or output normalization.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time


def digest(data):
    return hashlib.sha256(data).hexdigest()


def main():
    config = json.loads(Path(__file__).with_name('capture-config.json').read_text())
    binary = Path(config['binary'])
    assert digest(binary.read_bytes()) == config['binary_sha256'], 'runner changed'
    # The integration gate uses the same binary path for read-only delta replay.
    if sys.argv[1:2] == ['lens']:
        os.execv(str(binary), [str(binary), *sys.argv[1:]])
    body = os.environ['WEAVE_ISSUE_BODY'].encode()
    task = json.loads(body)
    argv = [str(binary), *sys.argv[1:]]
    assert sys.argv[1] == 'lens-script'
    outdir = Path(tempfile.mkdtemp(prefix='capture-', dir=config['output_dir']))
    (outdir / 'task.json').write_bytes(body)
    start = time.monotonic()
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, start_new_session=True)
    timed_out = False
    try:
        stdout, stderr = proc.communicate(timeout=60)
    except subprocess.TimeoutExpired:
        import signal
        os.killpg(proc.pid, signal.SIGKILL)
        stdout, stderr = proc.communicate()
        timed_out = True
    elapsed = time.monotonic() - start
    (outdir / 'stdout.bin').write_bytes(stdout)
    (outdir / 'stderr.bin').write_bytes(stderr)
    record = dict(argv=argv, binary_sha256=config['binary_sha256'],
                  body_sha256=digest(body), artifact_digest=task['artifact']['digest'],
                  run={k: os.environ.get(k) for k in ('WEAVE_ID', 'WEAVE_ISSUE', 'WEAVE_BASE', 'WEAVE_BRANCH')},
                  cwd=os.getcwd(), exit_code=proc.returncode, timed_out=timed_out,
                  runtime_seconds=elapsed, stdout_sha256=digest(stdout),
                  stderr_sha256=digest(stderr))
    (outdir / 'capture.json').write_text(json.dumps(record, indent=2) + '\n')
    sys.stdout.buffer.write(stdout)
    sys.stdout.buffer.flush()
    sys.stderr.buffer.write(stderr)
    sys.stderr.buffer.flush()
    return 124 if timed_out else (proc.returncode if proc.returncode >= 0 else 128 - proc.returncode)


if __name__ == '__main__':
    sys.exit(main())
