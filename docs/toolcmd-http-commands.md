# Typed HTTP tool commands

A tool's `commands:` entry with `mode: http` is a typed JSON request to a
service, run by `pkg/toolcmd` (`bashy tool cmd run TOOL:NAME -- ARGS...`).
No shell, `eval`, `curl` or per-command wrapper is involved. The declaration
is portable YAML data in the tool asset, and `pkg/fleet` validates it before
anything runs (`ToolCommand.ValidateCommands`).

## Schema

```yaml
commands:
  - name: decide                 # canonical command name (TOOL:decide)
    mode: http
    effects: [net, write]        # atlas effect atoms; mutating ones need --yes
    confirm: false               # true = need --yes even with no mutating effect
    timeout: 30s
    http:
      method: POST               # GET | POST | PUT | PATCH | DELETE
      url: http://127.0.0.1:8787 # loopback only unless effects has net AND remote
      path: /v1/decide           # optional; begins with /, no query or fragment
      token_ref: svc             # exactly one of token_ref | token_file
      body:                      # literal JSON object, encoded by encoding/json
        source: bashy
        request: {kind: tool}
      args:                      # typed caller arguments, in positional order
        - name: id               # also --id VALUE / --id=VALUE
          field: request.id      # dotted body path; default = name
          required: true
        - name: decision
          required: true
          enum:                  # closed projection: caller value -> JSON value
            allow: APPROVED
            deny: REJECTED
        - name: ttl
          type: integer          # string (default) | integer | boolean
          default: "60"          # applied when omitted; bound like caller input
          max_len: 8             # bytes; 0 = 4096
```

`bashy tool cmd run svc:decide --yes -- req-7 deny` sends

```json
{"source":"bashy","request":{"kind":"tool","id":"req-7"},"decision":"REJECTED","ttl":60}
```

## Argument binding (`http.args`)

- A caller word is `--name VALUE`, `--name=VALUE`, or positional. Positionals
  fill the arguments not bound by name, in declaration order. A bare `--`
  ends option parsing, so later words are positional even if they start
  with `--`.
- Every bound value is set as its own JSON value at its field. Two arguments
  are always two fields; a value that looks like JSON stays a string.
- `enum` accepts only its keys (case-sensitive) and sends the mapped value
  (string, number or boolean). `type` is a parse rule (`integer` = base-10
  int64, `boolean` = Go `strconv.ParseBool`); set `type` or `enum`, not both.
- An unknown `--name`, a repeated argument, a surplus positional, a missing
  `required` argument, an over-long value, or a value outside its enum or type
  is refused **before** the token is read or a connection is opened. This
  includes `--dry-run`, which checks the binding and sends nothing.
- Validation refuses: a bad name/field, two args whose fields overlap
  (`a` and `a.b`), a field that would overwrite a literal `body` value or
  descend through a non-object, `required` together with `default`, and
  `{args}` in a body that also declares `args`.

Without `http.args`, string values in `body` may use `{args}`, replaced
verbatim by the caller's whole text (the original single-argument form).

## Admission (`--yes`)

`fleet.ToolCommand.NeedsConfirmation` is the one admission rule, for every
mode: a command that declares a mutating effect (`write`, `destroy`,
`persist`, `priv`, `spend`) or `confirm: true` runs only with operator
confirmation (`toolcmd.Options.Yes`, the `--yes` flag). Without it,
`toolcmd.Run` and `StartJob` (`--async`) refuse with `refusal: confirm`
(`ErrNeedsConfirmation`) and nothing is sent. Front doors that offer `--yes`
call `toolcmd.Admit` or the fleet method rather than restating the rule.

## Transport safety

- The bearer token is read from `token_file` or resolved from `token_ref`
  through the host secret bindings. It is never placed in a result, error or
  log.
- Redirects are not followed: the response to the declared endpoint is the
  result, so neither the token nor the body can be forwarded to another host.
- Non-loopback URLs need declared `net` and `remote` effects.
