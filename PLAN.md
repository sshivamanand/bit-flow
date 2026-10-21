# bit-flow — Plan

Laptop → laptop transfer of 100+ GB (games, big files) over any Wi‑Fi or cable, as fast as the link allows.
Full design: [ARCHITECTURE.md](ARCHITECTURE.md) · diagram: [docs/architecture.png](docs/architecture.png)

## Requirements (from discussion)

- Devices send directly to each other (peer-to-peer); no central host needed.
- Host OS: any mix of macOS / Windows / Linux.
- File sizes: 100+ GB single files and game folders with many thousands of files.
- Both sides choose: sender picks what to send, receiver picks where it lands.
- Works on any Wi‑Fi; cables (Ethernet, Thunderbolt/USB4) used when present.
- Highly concurrent: multiple streams, worker pipelines, multipath.

## Decisions

- **Go**, single static binary, the same on every laptop.
- **Native peer-to-peer engine** with N parallel TCP streams (TLS 1.3), auto-tuned. The browser is only the UI and a fallback.
- 32 MiB chunks for big files, 32 MiB packs for small files, one shared work queue, XXH3-128 per block, resume journal.

## Milestones

### M0 — Skeleton ✅
- Go module, `cmd/bitflow`, subcommands stubbed (`send`, `recv`, `bench`).
- List interfaces + addresses; cross-compile targets in Makefile.

### M1 — Fast core (CLI first, prove the speed) ✅ (two-laptop link test pending)
- `bitflow recv --to <dir>` / `bitflow send <path> --to <ip>`.
- Control connection + N TCP data streams, frame codec.
- Planner (chunks), sender/receiver pipelines, buffer pool, files sized up front (`Truncate`) + `pwrite` at offset.
- XXH3-128 verify per block. No encryption/compression yet. (True `fallocate`/`F_PREALLOCATE` moves to M2.)
- `bitflow bench` (loopback + peer). **Exit criteria: ≥ 90% of raw link ceiling on a 20 GB file.**

M1 results (Apple M4, 2026-10-21):
- Engine ceiling, loopback, no disk: **6–15 GB/s** (best at 1–2 streams; more streams only add CPU contention on one machine).
- Disk → disk on the same SSD, 21.2 GB (20 GB file + 20,000 small files): **1.17 GB/s**, byte-identical.
  The SSD is doing read + write at once (~2.4 GB/s combined, vs 2.35 GB/s raw `dd` write), so this is disk-bound.
- Small files alone: ~10k files/s (590 MB/s at 32 streams). Packs in M2 will raise this.
- Still to do: run `bitflow bench --to <ip> --sweep` and a real send between two laptops on Wi-Fi + cable, and compare with iperf3.

### M2 — Robust ✅
- Small-file packs (≤ 1 MiB files batched into ≤ 4 MiB frames) + 16-worker parallel writer on the receiver.
- Resume journal (`~/Library/Caches/bitflow/resume/…` / `%LocalAppData%` / `~/.cache`), checkpointed every 3 s
  (snapshot → fsync → atomic write). Sender reconnects automatically (10 retries, backoff to 10 s), and the
  receiver replaces its own dead session when the same offer reconnects.
- Pairing: 6-digit code → CPace PAKE → both TLS 1.3 certificates pinned. 5 wrong codes rotate the code; each
  failure costs 1 s. `--raw` (sender) + `--allow-raw` (receiver): HMAC-authenticated, unencrypted data streams.
- Conflict policy `--on-conflict=error|skip|rename|overwrite`, identical files always skipped, free-space check,
  preallocation (`F_PREALLOCATE` / `fallocate`), keep-awake (`caffeinate` / `systemd-inhibit` / `SetThreadExecutionState`).
- 30 s stall detection on both sides (read/write deadlines + sender watchdog).

M2 results (Apple M4, 2026-10-21):
- Loopback engine, TLS 1.3: **5–6 GB/s** (2.8 GB/s on one stream); raw: ~9 GB/s. Encryption is free at real link speeds.
- 20,000 small files: 443 → **561 MB/s** at 8 streams with packs (TLS on); 662 MB/s at 32 streams. File creation is now the limit.
- 8 GB file + 20,000 small files, same SSD: 0.95 GB/s TLS, 1.09 GB/s raw.

### M3 — Discovery + UI ✅
- Discovery (`internal/discovery`): JSON query by UDP multicast (224.0.0.177, every interface) + directed and
  limited broadcast. Responders reply by unicast. Fallback: TCP probe of each local /24 (`KindProbe`).
  Responders always listen on UDP 7700 and advertise their real TCP port. Device ID + name live in the config dir.
- `bitflow peers`; `send --to NAME` resolves device names; `send` with no `--to` picks the only receiver found
  (or asks), and if none is found it **waits to be fetched** (`recv --from NAME`). Either side can listen, so one
  firewalled laptop is enough to make a transfer work.
- Engine refactor: connection setup (`link`: dialing vs. hub) is separate from session logic (send vs. receive).
  Receivers choose the destination per offer (`Decision.Dest`).
- Desktop app: `bitflow` with no arguments starts an agent (receiver + discovery + sender) and opens a web
  UI on `127.0.0.1:7710`. Vanilla HTML/JS embedded in the binary (no build step), Monkeytype-style
  monospace design with themes (flow = lime, ember, violet, paper). First run asks for the device name.
- Sessions: the launch URL's one-time token is traded for an HttpOnly, SameSite=Strict cookie. Sessions
  (hashed), theme, device name and transfer history persist across restarts. Host-header check
  (no DNS rebinding) and a custom header on writes (no CSRF).
- Native pickers without dependencies: `osascript` (macOS), PowerShell WinForms (Windows), zenity/kdialog (Linux).

### M4 — Turbo ✅
- **Adaptive zstd** (`klauspost/compress`, fastest level) per block, checksummed raw. Skips known-compressed
  formats and files whose first blocks don't shrink. Auto mode pauses after 64 MB windows that save < 5%
  (retries after 20 s), and stops for good when goodput passes 60% of zstd's measured capacity.
- **Auto-tuned streams**: all connections (16) open up front, and a gate lets only the first N pull work. N starts
  at 4 and doubles while each doubling buys ≥ 8% more throughput, then settles on the best. No protocol changes needed.
- **Multipath**: peers exchange interface lists. Extra links (different local interface, not VPN/virtual) are probed
  in parallel (700 ms) and streams are spread round-robin across all links that answer.
- **Topology advisor**: double air hop (both on Wi-Fi, same router), multipath in use, direct cable, and
  client isolation (found by discovery but unreachable). Shown in the CLI and the app.
- **Symlinks**: relative links inside the transfer are sent and created last, never written through. Links
  escaping the transfer are refused. Copied app bundles keep valid code signatures.

M4 results (Apple M4, 2026-10-21):
- Chrome.app (1.51 GB, 704 files incl. 7 framework symlinks): zstd kept **748 MB (49.5%)** off the wire, so a
  Wi-Fi-bound copy takes about half the time. The copy is identical and `codesign --verify --deep` passes.
- On loopback (no link bottleneck) compression costs time (1.6 s vs 0.6 s), which is the case auto mode turns off.
- Found and fixed: unreachable extra links stalled stream setup ~8 s (macOS drops rather than refuses).
  Links are now probed with a 700 ms timeout.

### M5 — Reach ✅
- **One port for everything**: `web.Split` peeks at each connection's first 4 bytes. `BFLW` goes to the transfer
  hub, an HTTP method goes to the web server. One firewall rule, one address to share.
- **`/get`**: any bit-flow serves itself plus `make cross` binaries found next to it or in `dist/`. There's a
  download page (OS-detected), `curl -fsSL http://IP:7700/get/sh | sh` (no Gatekeeper quarantine) and a
  PowerShell line for Windows.
- **Phones**: the same address shows a page. After entering the pairing code (rate-limited and rotated exactly like
  PAKE attempts), a phone can upload to a receiver (8 MB chunks, 4 in parallel, retried, out-of-order safe,
  never overwrites) or download from `send --wait` (Range-resumable files, folders as streamed store-only zips).
- **QR codes**: in the `recv` / `send --wait` banners (terminal half-blocks) and the app's **phone** panel (`p`).
- **Steam**: `bitflow games` lists installed games across all libraries (VDF parser, old and new
  `libraryfolders.vdf`, skips half-installed games). `send --steam NAME` sends `common/<dir>` + `appmanifest`,
  and the receiver drops them into its own Steam library with the new **`update`** policy (identical files
  skipped, changed files replaced), warning about OS mismatches. The app has a **game** button per device.

M5 results (2026-10-21): a 31 MB phone-page upload arrived byte-exact. The installer installed a working
binary. A game update re-sent only the one changed file (300 KB of a 3.3 MB game). E2E suite now at 23 checks.

### M6 — Polish ✅
- CI (GitHub Actions): vet + race tests on macOS and Linux, Windows tests (informational), gofmt, vet for every
  OS, cross-compile. Release workflow: a `v*` tag runs goreleaser → archives for 6 platforms + checksums.
- `bitflow history`: one log (`history.jsonl`, append-only) for CLI sends/receives, app transfers and phone uploads.
- `bitflow config [name|dest|on_conflict VALUE]`; `recv` falls back to the configured folder and policy.
- Themes (dark: flow/ember/violet, light: paper), README screenshot.

### M7 — The room ✅
- Any browser ↔ any browser on the Wi‑Fi, through whichever computer runs bit-flow (`internal/web/room.go`).
  Live device list over a WebSocket, a 6-digit code per tab (1 s per wrong guess, rotated after 5), accept
  prompt, pairing remembered both ways after one transfer, unique names, 30 s grace on reconnect.
- Data: sender `PUT`s each file, receiver's single download (zip for several) is fed straight from it. No temp
  file and nothing held in page memory, so big files work on phones. Cancel cuts both requests.
- Room page (`page.html`) keeps the host's code-protected upload/download and `/get`; invite QR (`/room/qr`).
  The app's phone panel links to it.

M7 results (Apple M4, 2026-10-21): 9 room tests (roster, origin check, wrong code + rotation, single file +
pairing, zip + key checks, decline, cancel mid-file, reconnect, unique names) pass 20× under `-race`. Checked in
a real browser: two tabs, wrong code, right code, accept, a 3 MB two-file zip delivered, reply without a code,
decline, and a 375 px phone viewport.

### M8 — Tidy history ✅
- Browser uploads arrive one file per request, so 300 photos made 300 rows. `history.Batches` groups them per
  top-level folder, or per run of loose files from one sender (2 min gap), for both the app list and
  `history.jsonl` (one line per batch, logged when it goes quiet or on shutdown). Old per-file rows merge on load.
- App: finished rows are one compact line; only the newest 5 show (**show N older**). **files** expands a
  batch's file list (up to 1,000 kept per row), **×** removes a row, **clear history** (two clicks) clears the
  list and the log. CLI: `bitflow history --clear`.

M8 results (2026-10-22): ten phone uploads (six photos + a four-file folder) showed as two rows. Expand,
remove and clear checked in the app; tests cover grouping, gaps, reload, old-row merge, remove and clear.

### M9 — One nearby list ✅
- The app joins the room in-process (`web.Local`): phones and browsers appear under **nearby** next to
  bit-flow computers (hollow dot, "browser"). Computers keep the direct encrypted path; only browsers use
  the room.
- Phone → app: its code is the app's pairing code (same rate limit and rotation), the normal accept prompt,
  and files written into the receive folder (`SafeJoin`, `.bitflow-partial`, never overwriting). App → phone:
  pick files/folder, type the code on the phone's screen (skipped once paired).
- UI polish: the big room button became a quiet **+ invite** row with **show qr**; tidier rows, mobile layout,
  "invite" in the nav; the room page lists the app as a device and drops the duplicate upload box.

M9 results (2026-10-22): `TestAppInRoom` (phone → app → folder, app → phone, pairing) plus `web.Local` tests
pass under `-race`. Checked in a real browser: accept prompt for a phone send, files on disk, folder back as a zip.

## Still to do (needs two real laptops)
- Run `bitflow bench --to <ip> --sweep` and a real game transfer over Wi‑Fi, Wi‑Fi + Ethernet, and a
  Thunderbolt cable, and record the numbers here (link ceiling vs. bit-flow).
- Try the app, native dialogs and Steam paths on real Windows hardware (the full test suite passes on Windows CI,
  after it caught a resume-key bug, an upload handle leak and two fixture bugs; all fixed in v0.1.1).
