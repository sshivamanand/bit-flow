# CLAUDE.md

Guidance for Claude Code (and humans) working in this repository.

## What this is

**bit-flow** moves very large amounts of data (100+ GB game folders, disk images) between laptops on
the same local network as fast as the physical link allows. One static Go binary runs on both ends.
The design rationale lives in [ARCHITECTURE.md](ARCHITECTURE.md), and the milestone roadmap with
measured results in [PLAN.md](PLAN.md). Read both before large changes. When the design changes,
update them (and `docs/architecture.svg` → re-render `docs/architecture.png`, see below).

Repo: `github.com/sshivamanand/bit-flow` (Go module path is the same). Author: Shivam Anand.

## Docs

User-facing docs live in `docs/` (index: `docs/README.md`; screenshots in `docs/img/`). When behavior,
flags or defaults change, update the matching guide (`cli.md` for flags, `app.md` for the UI, `protocol.md`
for wire changes, `security.md` for anything touching trust) in the same change. Get flag text from
`bitflow <cmd> -h` rather than from memory, and re-run the relative-link check in the docs commit.

## Commands

```bash
make build                     # bin/bitflow (stripped, -trimpath)
make test                      # go test -race ./...
make bench                     # loopback engine ceiling, stream sweep
make e2e                       # real binary, 23 user-facing flows (macOS, ~1 min, ~10 GB sparse temp data)
make cross                     # dist/bitflow-<os>-<arch>[.exe] for darwin/windows/linux × arm64/amd64
go vet ./... && GOOS=windows go vet ./...   # always vet Windows too: per-OS files exist
gofmt -l .                     # must print nothing
```

End-to-end by hand (two terminals, same machine). Give each side its own config dir, or discovery
treats them as the same device and ignores the other:

```bash
BITFLOW_CONFIG_DIR=/tmp/a bin/bitflow recv --to /tmp/in --yes --port 7711   # prints code + name
BITFLOW_CONFIG_DIR=/tmp/b bin/bitflow send ./some/folder --code <code>      # finds it by discovery
BITFLOW_CONFIG_DIR=/tmp/b bin/bitflow ui --no-browser                       # app; open the printed URL
```

Avoid clicking "files…/folder…" in the UI during automated checks. That opens a real native dialog on the
user's screen. Drive `/api/send` with the launch token instead (`X-Bitflow-Token` + `X-Bitflow: 1`).

Re-render the diagram after editing `docs/architecture.svg`:

```bash
cd docs && "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu \
  --hide-scrollbars --force-device-scale-factor=2 --window-size=1600,1050 \
  --screenshot="$PWD/architecture.png" "file://$PWD/architecture.svg"
```

## Layout

| Path | Role |
|------|------|
| `cmd/bitflow` | CLI only: flag parsing, prompts, progress meter. No transfer logic here. |
| `internal/proto` | Wire format: Hello, control messages (length-prefixed JSON), 33-byte data frame header, pack encoding, XXH3 checksum. **Bump `proto.Version` on any wire change.** |
| `internal/secure` | Pairing (CPace PAKE over a 6-digit code), ephemeral TLS identities, fingerprint pinning, raw-mode stream tokens. |
| `internal/planner` | Sender side: walk paths → `Manifest`; split into work `Unit`s (32 MiB chunks of big files, packs of small files), honoring resume info. |
| `internal/store` | Receiver side: path safety, conflict policy, preallocation, out-of-order `WriteAt`, packs, `.bitflow-partial` + rename, resume journal, free-space checks. |
| `internal/transfer` | The engine: `Sender` / `Receiver`, control session, N data streams, pipelines, retries, compression (`compress.go`), stream gate + tuner (`tuner.go`), multipath (`paths.go`). |
| `internal/web` | Browser side of the transfer port: `Split` (peer vs HTTP by first bytes), `/get` + installer, phone upload/download pages, QR (terminal + SVG). |
| `internal/steam` | Steam libraries and installed games (VDF parser); `BITFLOW_STEAM_ROOT` points it at a fake install in tests. |
| `internal/history` | Append-only `history.jsonl` shared by CLI and app (`bitflow history`). |
| `internal/advisor` | Plain-English advice from the route (double air hop, multipath, cable, client isolation). |
| `internal/netinfo` | Interface listing with friendly labels (Wi-Fi, Ethernet, Thunderbolt Bridge). |
| `internal/discovery` | Find peers: UDP multicast + broadcast query, unicast answers, TCP /24 probe fallback, name lookup. |
| `internal/config` | Device ID, name, default folder, conflict policy (`config.json` in the user config dir; `BITFLOW_CONFIG_DIR` overrides it). |
| `internal/ui` | Desktop app: agent (`App`), loopback HTTP API + SSE (`Server`), sessions, history, native pickers, embedded `static/index.html`. |
| `internal/awake` | Keep the machine awake during transfers (per-OS files). |
| `internal/units` | Size parsing/formatting. |

`internal/transfer` is split into `link.go` (who dials and who listens: `dialLink`, `hub`), `sender.go` and
`receiver.go` (session logic, independent of who dialed), and `common.go`. Check PLAN.md for
what has landed per milestone.

## Invariants (don't break these)

- **Every connection starts with `proto.Hello`.** Control connections then pair (CPace) and upgrade to
  TLS 1.3 pinned to the exchanged fingerprints. Data connections are TLS with the same pins (`KindData`), or
  `KindDataRaw` with an HMAC token from the pairing key. `KindProbe` only returns public discovery info.
  No unauthenticated path to files exists. Keep it that way.
- **Browser pages on the LAN port** need the pairing code (`hub.checkCode`, same penalty and rotation as PAKE)
  before any upload/download. Uploads go through `store.SafeJoin` and never overwrite. Only `/`, `/get*` and
  `/web/info` are public.
- **The UI server only listens on loopback**, checks the Host header, needs a session cookie or the launch
  token, and requires `X-Bitflow: 1` on writes. Never store raw session IDs (hash them).
- **Receiver never trusts sender paths.** Everything goes through `store.SafeJoin`. Never write outside `Dest`.
- **Existing files are never modified** unless the conflict policy says so (`overwrite`). Identical files
  (same size, mtime within 2 s) are skipped.
- **Files are written as `<name>.bitflow-partial` and renamed only when complete.** A final path that
  exists with the right size/mtime is treated as done by resume logic, so never rename early.
- **The resume journal must never claim bytes that aren't durable.** Checkpoints snapshot ranges, then
  fsync, then atomically write the journal.
- **Checksums are verified before `WriteAt`.** Blocks are hashed raw (before compression); compressed frames
  carry `RawLen`, which must be ≤ the block size and match the decompressed length.
- **The resume key (`store.JournalKey`) covers files only** (path, size, mtime, link), never directories. Directory
  mtimes change on their own (Finder, Windows metadata) and once broke resume on Windows. The planner skips
  `.DS_Store`, `Thumbs.db` and `desktop.ini` for the same reason.
- **Symlinks are created last and never written through** (`store.checkLinks`). Never relax `planner.LinkInside`.
- **Any network probe of an extra path needs a short timeout.** Some networks drop instead of refuse;
  a 10 s dial on a dead path once added 8 s to every transfer.
- **Pipelines use bounded channels and pooled buffers.** No unbounded queues or per-block allocations on
  the hot path. Buffers carry `FrameHeaderSize` bytes of headroom so header+payload go out in one write.
- **Cancellation:** one `context.WithCancelCause` per session; any stream error cancels the session,
  and `closeOnDone` closes sockets so blocked I/O returns. The first cause wins and is reported.

## Conventions

- **UI style**: Monkeytype-like. Monospace everywhere, lowercase copy, one accent colour per theme (CSS variables
  under `[data-theme=…]`), big numbers with small muted labels, keyboard shortcuts. No external fonts, scripts or
  CDNs: the app must work offline on a LAN. Keep `index.html` a single self-contained file.

- Go standard library first. Current deps: `zeebo/xxh3`, `filippo.io/cpace`, `klauspost/compress` (zstd),
  `golang.org/x/sys`, `golang.org/x/net` (multicast), `rsc.io/qr`. Native dialogs shell out to the OS (no zenity). Justify any new dependency (speed, correctness, or pure-Go portability). Builds
  must stay `CGO_ENABLED=0` for all six targets.
- Per-OS code goes in `_darwin.go` / `_linux.go` / `_windows.go` / `_other.go` files with matching build
  constraints, never `runtime.GOOS` switches on hot paths.
- Comments explain *why* (performance reasoning, protocol constraints), not what. Package docs carry the design summary.
- Errors shown to users are plain English and actionable ("…already exist at the destination (receiver
  can use --on-conflict=rename)"). Wrap with `%w`; mark non-retryable failures with `transfer.permanent`.
- Sizes printed to users are decimal (GB, MB/s), matching how drives and links are sold.

## Testing

- `internal/transfer/transfer_test.go` runs real sender↔receiver transfers over loopback with small
  chunk/block sizes, so files split into many units across streams. Add an end-to-end test for every
  behavior change (resume, conflicts, packs, raw mode, wrong code, …).
- Always run with `-race`. Tests must be deterministic: don't race a transfer against a timer;
  drive the protocol directly (see `TestSenderDisappears`) when you need a mid-transfer failure.
- Benchmarks: `bitflow bench --loopback --sweep` (engine), and for disks, a generated 20 GB file plus
  20k small files sent over 127.0.0.1. Record numbers in PLAN.md with date + hardware.
  Put big test data in the scratchpad or `/tmp`, and delete it afterwards.

## Performance notes (measured, Apple M4, 10 cores, 16 GB)

- XXH3-128: ~35 GB/s/core. BLAKE3 (Go): 1–5 GB/s. SHA-256: 3.3 GB/s. That's why blocks use XXH3.
- Engine on loopback, no disk: 6–15 GB/s plain TCP. More streams only add CPU contention on one machine.
- Same-SSD disk→disk: ~1.2 GB/s (SSD doing read + write at once, ~2.35 GB/s raw write).
- Over Wi-Fi the radio is the ceiling (~30–150 MB/s). The real wins are multipath, avoiding the
  double air hop (one laptop wired to the router), and compression when the link is the bottleneck.

## Platform gotchas

- macOS `File.Sync` is `F_FULLFSYNC` (slow), so only checkpoint every few seconds.
- Windows NTFS: writing far past a file's valid-data length zero-fills the gap. Chunks are scheduled
  largest-file-first with consecutive offsets so writes stay near-sequential. Don't randomize chunk order.
- Windows Defender scans each new file; small-file throughput there is far lower than on macOS.
- macOS may show a firewall prompt the first time `recv` listens; Windows shows "Allow access". Allow it on Private networks.

## CI and releases

- `.github/workflows/ci.yml`: push to `main` and PRs. Race tests on macOS + Linux, plain tests on Windows (all
  required), gofmt, vet for every OS, `make cross`. Windows CI caught a real resume bug, a handle leak and two
  fixture bugs, so keep it required. The repo is private: macOS minutes bill 10×,
  so don't add triggers casually.
- `.github/workflows/release.yml` + `.goreleaser.yaml`: pushing a `vX.Y.Z` tag builds archives for all six
  targets with `main.version` set. Only tag after `make test`, `make e2e` and CI are green.

## Git workflow

- One branch per milestone (`m2-robust`, `m3-discovery-ui`, …), merged into `main` with `--no-ff`
  after `make test`, vet (incl. Windows), gofmt and a cross build pass. Then push `main`.
- Commit messages: imperative summary line, a body with bullet points, and the `Co-Authored-By` trailer.
- Update PLAN.md (✅ + results) and this file in the same change as the feature.
