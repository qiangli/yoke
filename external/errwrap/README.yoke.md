# external/errwrap — clean-room errwrap (S488.3c)

Original yoke code, permissive under the repository `LICENSE`. Written
clean-room for Sprint 488 story S488.3c: the MPL-2.0
`hashicorp/errwrap` source was never opened. Nothing in the yoke engine
graph imports errwrap directly — the module was only reached as a
dependency of `hashicorp/go-multierror`, which yoke now replaces with
`external/multierror` (whose implementation does not need it). This
module exists so the `github.com/hashicorp/errwrap` module path still
resolves to permissive code:

```
replace github.com/hashicorp/errwrap => ./external/errwrap
```

The surface is the small wrapping/walking core (`Wrap`, `Wrapf`, `Walk`,
`Get`, `GetAll`), designed from the standard library error-wrapping
conventions (`fmt.Errorf` with `%w`, `errors.Is`/`errors.As`
traversal). If a future caller needs more API, extend this file
clean-room from that caller's use (permissive sources only).

The nested module declares `github.com/qiangli/yoke/external/errwrap`
so standalone consumers can fetch it at a yoke commit and use it on the
right side of their own `replace github.com/hashicorp/errwrap => ...`
directive, the same arrangement as `external/go-billy`.
