# Development

bit-flow is plain Go (no cgo) with a vanilla HTML/JS front end embedded in the binary. No Node, no build
step for the UI.

## Build and test

```bash
./bitflow       # from the repo root: builds on first use, then runs (e.g. ./bitflow recv --to ~/Downloads)
make build      # bin/bitflow
make test       # go test -race ./...   (11 packages)
make e2e        # 23 end-to-end checks of the real binary (macOS; ~1 min, uses ~10 GB of sparse temp data)
make bench      # engine ceiling on loopback, 1–32 streams
make cross      # dist/bitflow-{darwin,linux,windows}-{amd64,arm64}
```

Before committing: `gofmt -l .` must print nothing, and `go vet ./...` must pass for every OS (`GOOS=windows
go vet ./...`, `GOOS=linux …`), because there are per-OS files.

### Running two copies on one machine

Discovery ignores devices with your own ID, so give each copy its own config folder:

```bash
BITFLOW_CONFIG_DIR=/tmp/a bin/bitflow recv --to /tmp/in --port 7711 --yes
BITFLOW_CONFIG_DIR=/tmp/b bin/bitflow send ./something --code <code>
BITFLOW_CONFIG_DIR=/tmp/b bin/bitflow ui --no-browser
```

`BITFLOW_STEAM_ROOT` points Steam discovery at a fake install (the tests build one).

## Code layout

| Path | |
|---|---|
| `cmd/bitflow` | CLI only: flags, prompts, progress meter |
| `internal/proto` | wire format (Hello, control messages, frames, packs, checksum) |
| `internal/secure` | CPace pairing, ephemeral TLS identities, pinning, raw tokens |
| `internal/planner` | sender: walk paths → manifest → work units (chunks, packs), Steam layout |
| `internal/store` | receiver: path safety, conflicts, preallocation, `WriteAt`, resume journal, symlinks |
| `internal/transfer` | engine: `link.go` (dial vs. listen), `sender.go`, `receiver.go`, `compress.go`, `tuner.go`, `paths.go` |
| `internal/discovery` | UDP multicast/broadcast discovery, TCP probe fallback |
| `internal/web` | browser side of the transfer port: port split, `/get`, phone pages, QR codes |
| `internal/ui` | the app: agent, loopback API + SSE, sessions, history, native pickers, `static/index.html` |
| `internal/steam` | Steam libraries and games (VDF parser) |
| `internal/advisor` | route-based advice |
| `internal/config`, `history`, `netinfo`, `awake`, `units` | settings, transfer log, interface names, keep-awake, sizes |
| `scripts/e2e-macos.sh` | the end-to-end suite |

[ARCHITECTURE.md](../ARCHITECTURE.md) explains the design, [protocol.md](protocol.md) the wire format, and
[CLAUDE.md](../CLAUDE.md) the invariants and conventions to keep (written for AI assistants, but just as
useful to people).

## Testing approach

- `internal/transfer/*_test.go` runs real sender↔receiver transfers over loopback with small chunk and block
  sizes, so every file splits into many units across streams. Every behavior change gets an end-to-end test
  there: resume, conflicts, packs, raw mode, wrong codes, reconnects through a proxy that cuts connections,
  multipath, Steam.
- Tests are deterministic. Mid-transfer failures are produced by driving the protocol directly or through the
  flaky proxy, never by racing a timer.
- `make e2e` drives the real binary through every user-facing flow, including the app's HTTP API and
  phone uploads.
- UI changes are checked in a real browser. Don't trigger the native file pickers in automated checks:
  they open real dialogs on your screen. Drive `/api/send` instead.

## CI and releases

- **CI** (`.github/workflows/ci.yml`) runs on pushes to `main` and on pull requests: race tests on macOS
  and Linux, tests on Windows, gofmt, vet for every OS, and the cross build. Windows CI has caught real
  bugs (resume keys, a file-handle leak), so it's required.
- **Releases**: push a tag `vX.Y.Z`. `.github/workflows/release.yml` runs the tests and goreleaser, which
  publishes archives for all six platforms plus `checksums.txt`, with `bitflow version` set from the tag.
  Tag only after `make test`, `make e2e` and CI are green.

## Dependencies

Standard library first. Current dependencies, each for a reason:

| Module | Why |
|---|---|
| `github.com/zeebo/xxh3` | block checksums at ~35 GB/s per core |
| `filippo.io/cpace` | the PAKE behind pairing codes |
| `github.com/klauspost/compress` | zstd, pure Go and fast |
| `golang.org/x/sys` | preallocation, free space, socket options |
| `golang.org/x/net` | multicast group membership on every interface |
| `rsc.io/qr` | QR codes |

Builds must stay `CGO_ENABLED=0` for all six targets.
