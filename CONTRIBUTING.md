# Contributing

Thanks for helping! Bug reports, device support, translations and UI polish are all welcome.

## Development setup

```sh
go run . demo -listen 127.0.0.1:8080   # full UI against fake backends, password "glovebox"
go test ./...
```

The web UI in `web/` is plain HTML, CSS and JavaScript with no build step and no dependencies;
reload the page after editing (restart `go run` since files are embedded).

To try changes on a device: `./build.sh && GLOVEBOX_PASSWORD_FILE=pw.txt ./deploy.sh`.

## Project layout

| Path | Contents |
| --- | --- |
| `main.go` | Command line: `serve`, `passwd`, `config`, `demo`, `version` |
| `server.go` | HTTP/HTTPS listeners, routes and the access guard |
| `api*.go` | JSON API handlers, grouped by page (status, proxy, device, logs) |
| `zte.go`, `mihomo.go`, `sys.go` | Clients for the vendor web API, mihomo and `/proc` / `/sys` |
| `auth.go`, `config.go`, `tls.go` | Passwords and sessions, configuration file, local CA |
| `demo.go` | Fake backends for demo mode and tests |
| `web/` | The embedded UI (`app.js`, `style.css`, `i18n/`, icons, fonts, service worker) |
| `module/`, `overlay/` | Magisk module scripts and the SoftAP client-limit overlay |

Build output (`dist/`, the overlay APK and its keystore) is ignored by git; never commit binaries.

## Guidelines

- Run `gofmt`, `go vet ./...` and `go test ./...` before sending a pull request.
- Source code, comments, logs and API messages are **English only**. User-visible UI text goes
  through `t('key')` with entries in every file under `web/i18n`; the tests enforce both.
- API errors return a stable `code` (and optional `args`) next to the English message; add an
  `err.<code>` entry to each locale when you introduce a new code.
- Keep the UI usable on small and coarse-pointer screens; check the phone and landscape layouts
  in demo mode.
- Device-specific behaviour belongs behind a small client (see `zte.go`) so other devices can be
  supported without touching the UI.

## Adding a translation

1. Copy `web/i18n/en.json` to `web/i18n/<code>.json` (BCP 47, e.g. `de`, `zh-TW`).
2. Translate the values; keep `{placeholders}` and any HTML tags unchanged.
3. Add `"<code>": "<native name>"` to `web/i18n/index.json`.
4. Run `go test ./...`.

To translate the README, copy `README.md` to `README.<code>.md`, keep its headings, tables, code
blocks and screenshots, link it from the language line at the top of every README, and add
screenshots in that language under `docs/screenshots/<code>/` (take them in demo mode with the UI
switched to the language). `go test` checks that the structure matches.
