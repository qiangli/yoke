---
id: a76299332d33
kind: bug
title: ollama serve inherits the door client OLLAMA_HOST and binds the model door port 24556, so the door never starts
seq: 31
status: todo
priority: p1
labels:
    - genie
    - llm
created: 2026-10-08T21:00:39.207296Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Found by the S379 conductor on Dragon 2026-10-08: genie failed 'the model door did not start'; door.log repeats 'broker: listen on port 24556: bind: address already in use'. The holder is an orphaned 28h-old bashy-cached ollama v0.31.1 'ollama serve' (PPID 1) whose env has OLLAMA_HOST=http://127.0.0.1:24556/k/<owner token> - the door CLIENT URL that external/ollama applyClientEnv sets (door.OllamaHost). A serve that inherits that value binds the door's own port. Paths: external/ollama/managed.go applyManagedEnv keeps any inherited OLLAMA_HOST ('explicit override wins'); external/ollama/cmd.go newOllamaServeCmd system-binary path execs 'ollama serve' with the inherited env. Fix (KISS): a serve never binds a door client address - when OLLAMA_HOST points at the door (host port == door.Port() or a /k/ path), treat it as unset for serve and bind the managed port; apply to both the embedded and the system-binary serve (set c.Env explicitly). Red/green unit test: with OLLAMA_HOST=door.OllamaHost() set, the serve bind address is the managed port, never door.Port(); a genuine user OLLAMA_HOST (e.g. 127.0.0.1:11500) is still honoured. Do not kill the orphan on Dragon; the conductor handles it.

Worker rules (Sprint 379 conductor, 2026-10-08): work in the submodule named above; commit and push INSIDE it to its default branch (public repos: main), then stop - the conductor bumps the umbrella pin. Every commit carries Sprint: #379 plus Story/Story-ID trailers for this story, with BASHY_AGENT set to your own binding. Reproduce first with a red unit test, fix at the root, never skip, quarantine or add to a known-failures list. This dev box (Dragon) is for builds and focused unit tests only. Windows runs go to noviwin1.local, Linux runs to the novidesign.local podman machine bashy or the repo CI, and macOS full suites to novidesign.local (all passwordless ssh; use the .local names). Gate before push: go build ./... and go vet on the module, plus the focused tests, with the exit code captured (never gate a push on a pipe). Report: the root cause, the commit SHA, the exact tests run and where, and any CI run URL.
