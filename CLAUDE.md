# CLAUDE.md

Guidance for Claude Code (and humans) working in this repository.

## What this is

**bit-flow** moves very large amounts of data (100+ GB game folders, disk images) between laptops on
the same local network as fast as the physical link allows. One static Go binary runs on both ends.
The design rationale lives in [ARCHITECTURE.md](ARCHITECTURE.md), and the milestone roadmap with
measured results in [PLAN.md](PLAN.md). Read both before large changes. When the design changes,
update them (and `docs/architecture.svg` → re-render `docs/architecture.png`, see below).

Repo: `github.com/sshivamanand/bit-flow` (Go module path is the same). Author: Shivam Anand.

## Commands

```bash
make build                     # bin/bitflow (stripped, -trimpath)
make test                      # go test -race ./...
make bench                     # loopback engine ceiling, stream sweep
make cross                     # dist/bitflow-<os>-<arch>[.exe] for darwin/windows/linux × arm64/amd64
go vet ./... && GOOS=windows go vet ./...   # always vet Windows too: per-OS files exist
gofmt -l .                     # must print nothing
```

End-to-end by hand (two terminals, same machine):

```bash
bin/bitflow recv --to /tmp/in --yes            # prints the pairing code + addresses
bin/bitflow send ./some/folder --to 127.0.0.1 --code <code>
```

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
| `internal/transfer` | The engine: `Sender` / `Receiver`, control session, N data streams, pipelines, retries. |
| `internal/netinfo` | Interface listing with friendly labels (Wi-Fi, Ethernet, Thunderbolt Bridge). |
| `internal/awake` | Keep the machine awake during transfers (per-OS files). |
| `internal/units` | Size parsing/formatting. |

Later milestones add `internal/discovery`, `internal/ui` (embedded web UI), `internal/web` (browser
fallback + `/get` bootstrap), and `internal/steam`. Check PLAN.md for what has landed.

## Invariants (don't break these)

- **Every connection starts with `proto.Hello`.** Control connections then pair (CPace) and upgrade to
  TLS 1.3 pinned to the exchanged fingerprints. Data connections are TLS with the same pins, or, in raw
  mode, authenticated with an HMAC token from the pairing key. No unauthenticated path exists. Keep it that way.
- **Receiver never trusts sender paths.** Everything goes through `store.SafeJoin`. Never write outside `Dest`.
- **Existing files are never modified** unless the conflict policy says so (`overwrite`). Identical files
  (same size, mtime within 2 s) are skipped.
- **Files are written as `<name>.bitflow-partial` and renamed only when complete.** A final path that
  exists with the right size/mtime is treated as done by resume logic, so never rename early.
- **The resume journal must never claim bytes that aren't durable.** Checkpoints snapshot ranges, then
  fsync, then atomically write the journal.
- **Checksums are verified before `WriteAt`.** Blocks are hashed raw (before compression).
- **Pipelines use bounded channels and pooled buffers.** No unbounded queues or per-block allocations on
  the hot path. Buffers carry `FrameHeaderSize` bytes of headroom so header+payload go out in one write.
- **Cancellation:** one `context.WithCancelCause` per session; any stream error cancels the session,
  and `closeOnDone` closes sockets so blocked I/O returns. The first cause wins and is reported.

## Conventions

- Go standard library first. Current deps: `zeebo/xxh3`, `filippo.io/cpace`, `klauspost/compress`,
  `golang.org/x/sys`. Justify any new dependency (speed, correctness, or pure-Go portability). Builds
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

## Git workflow

- One branch per milestone (`m2-robust`, `m3-discovery-ui`, …), merged into `main` with `--no-ff`
  after `make test`, vet (incl. Windows), gofmt and a cross build pass. Then push `main`.
- Commit messages: imperative summary line, a body with bullet points, and the `Co-Authored-By` trailer.
- Update PLAN.md (✅ + results) and this file in the same change as the feature.
