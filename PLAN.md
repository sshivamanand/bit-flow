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

### M5 — Reach
- `/get` bootstrap page serving binaries for every OS.
- Browser fallback mode for phones.
- Game mode (Steam library detection + appmanifest).

### M6 — Polish
- Releases (goreleaser), transfer history, settings, dark mode.
