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

### M2 — Robust
- Small-file packs + parallel writer pool (game folders).
- Resume journal + reconnect.
- PAKE pairing + pinned TLS 1.3 on all streams; `--raw` for trusted cables.
- Conflict policy, skip-identical, free-space check, keep-awake.

### M3 — Discovery + UI
- Multicast / mDNS / subnet scan / QR; either side can listen.
- Local web UI (Svelte, embedded): device list, send dialog, accept dialog with destination picker, live MB/s + ETA.
- Native dialogs via zenity.

### M4 — Turbo
- Auto-tuner (stream count, compression, disk backpressure).
- Adaptive zstd.
- Multipath striping across Wi‑Fi + Ethernet + Thunderbolt.
- Topology advisor (double hop, client isolation).

### M5 — Reach
- `/get` bootstrap page serving binaries for every OS.
- Browser fallback mode for phones.
- Game mode (Steam library detection + appmanifest).

### M6 — Polish
- Releases (goreleaser), transfer history, settings, dark mode.
