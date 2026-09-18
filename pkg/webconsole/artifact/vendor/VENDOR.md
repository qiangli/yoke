# Vendored browser assets

Committed build output, not source. Each file is copied verbatim from the
published npm tarball; nothing here is edited by hand, and nothing here is built
from this repo.

| file | package | version | license |
|---|---|---|---|
| `xterm.js`, `xterm.css` | `@xterm/xterm` | 6.0.0 | MIT (`LICENSE.xterm`) |
| `xterm-addon-fit.js` | `@xterm/addon-fit` | 0.11.0 | MIT (`LICENSE.xterm-addon-fit`) |

Both are the UMD builds, so they define the `Terminal` and `FitAddon` globals
and need no bundler. That is the whole reason the console has no build step: the
page is plain HTML/CSS/JS that loads these two files, so `go build` alone
produces a working console and there is no `pnpm install` between a checkout and
a running binary.

Source maps are deliberately NOT vendored — 3.6 MB of them, for assets we do not
debug.

To refresh:

    npm pack @xterm/xterm@<version> @xterm/addon-fit@<version>
    # copy lib/xterm.js, css/xterm.css, lib/addon-fit.js, and both LICENSE files

Update the table above in the same commit, and check the licenses are still MIT.
