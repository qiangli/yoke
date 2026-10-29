---
name: dual
description: A dual-bundle fixture with a canonical face and bindings.
metadata:
  requires: os == linux
  check-tests: go test ./...
  step-reada: cat SKILL.md
  check: test -f SKILL.md
  author: fixture
  derived-from: h29588646852213ba6a20e2b51a9dae2de3476432f21a5fe1dd8154894d51b70d
---

# dual

Prose face. The executable unit is the Bash# body below; skill.dhnt beside
this file is its derived face (metadata.derived-from names the body).

```bsh
gereeni() { sh scripts/check.sh; }

@effects("read,write")
@ensure("gereeni")
reada() { cat SKILL.md >/dev/null; }

reada
```
