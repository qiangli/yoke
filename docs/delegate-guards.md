# Guarded delegation

`bashy delegate` uses the existing registered chat launcher. A caller can constrain
it without adding a model-facing tool:

```sh
BASHY_DELEGATE_CALLER=genie \
BASHY_DELEGATE_ALLOW=claude:opus5,codex:gpt-6-astra \
  bashy delegate codex:gpt-6-astra --timeout=10m --idle-timeout=90s \
  --json 'Review the change and report findings'
```

Allow entries match the resolved registry `tool:model` binding exactly (including
nickname resolution), not executable paths or glob patterns. `--allow` overrides
`BASHY_DELEGATE_ALLOW`, including an explicitly empty value. Empty policy refuses
a genie caller; it imposes no target restriction for other callers. Policy is
checked before the vendor runs. Budget routing cannot change the binding.

The `bashy-chat-v1` delegate envelope retains `exit_code` and adds `exit`,
`receipt`, `identity`, and an optional `error` containing `kind` and `message`.
Every failure has a nonzero exit. Kinds are `auth`, `timeout`, `idle`, `budget`,
`denied`, and `vendor`. For genie callers the identity is
`genie-delegate:<tool>-<model>` using registry names. Other callers have an empty
identity. An enclosing attestation launcher supplies its receipt ID through
`BASHY_ATTEST_ID`; delegate copies it without inventing an attestation or reading
an attestation store. Without that exported ID, `receipt` is empty.

`--timeout` bounds admission, authentication, and execution. `--idle-timeout`
measures the time since either child output stream last wrote bytes, following
`agentpty.IdleTimeout` semantics; input does not reset it. Cancellation tears down
the child process tree. Zero disables a watchdog; negative durations are refused.

`--budget-usd` and `--max-tokens` request hard caps. Currently both adapters refuse
any explicitly requested cap with `error.kind=budget`, before launch: a vendor
CLI's internal tool loop has no verified hard accounting contract. Text estimates
and final usage reports cannot enforce an in-flight ceiling. No cap flag means
that only the existing shared budget admission policy applies.

The initial subscription adapters are Claude and Codex. Before a task starts,
they run `claude auth status --json` or `codex login status` with a five-second
probe timeout and require subscription login evidence. Unknown adapters fail
closed with `auth`. Probe output is bounded and withheld from the result. The
launcher never opens vendor credential files; vendor CLIs own their login state.

Both the probe and task receive a scrubbed environment, after recipe overlays.
Metered API keys, bearer auth overrides, cloud billing credentials, and provider
endpoint overrides are removed. Declared credential-bearing bindings are denied.
The launcher also pins the subscription lane in vendor arguments: Claude loads
no user/project/local settings and uses `forceLoginMethod=claudeai` with an empty
key helper; Codex uses `forced_login_method=chatgpt` and `model_provider=openai`.
Vendor-managed policy remains authoritative. There is no retry through a metered
binding. These guards govern the launch; they do not replace the vendor sandbox
or prevent an authorized child from editing files or executing commands.

Vendor switch references: [Claude CLI](https://code.claude.com/docs/en/cli-reference),
[Claude settings](https://code.claude.com/docs/en/settings-reference), and
[Codex configuration](https://developers.openai.com/codex/config-reference/).

Validation uses fake binaries for each adapter, including failed login, metered
login, allow refusal, required caps, wall/idle timeout, stderr activity, vendor
failure, and environment overlays. Run `go test ./pkg/chat/...`.
