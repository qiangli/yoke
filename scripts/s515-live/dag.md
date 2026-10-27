---
name: s515-live
type: dag
---

# S515.2 peer storage acceptance

Run from the peer scratch directory after the README setup.

## Tasks

### live
Push and pull an OCI image and LFS object through real loom and peer storage.
Effects: exec,net,write

```bash
set -e
./bashy-s515-fixed.exe go run stack_windows.go acceptance_windows.go
```
