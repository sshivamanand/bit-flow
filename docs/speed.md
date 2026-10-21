# Speed guide

No software can make radio waves faster. What bit-flow can do is keep the link completely full, send fewer
bytes, use every link available, and tell you which setup change would help most. This page explains where
the time goes and how to win it back.

## 1. The link sets the ceiling

| Link | Real-world throughput | 100 GB takes |
|------|---:|---:|
| Wi‑Fi 5, both laptops on the same router | 30–40 MB/s | 45–55 min |
| Wi‑Fi 6, both laptops on the same router | 50–90 MB/s | 20–35 min |
| Wi‑Fi 6, **one laptop wired to the router** | 100–150 MB/s | 11–17 min |
| Gigabit Ethernet cable between the laptops | ~112 MB/s | ~15 min |
| 2.5 GbE USB adapters | ~280 MB/s | ~6 min |
| Thunderbolt / USB4 cable | 1–2.5 GB/s | 1–2 min (the disks become the limit) |

### The double air hop

When both laptops use Wi‑Fi through the same router, every byte crosses the air **twice** (laptop →
router → laptop), and both hops share the same airtime. That roughly halves the speed. Plugging **either**
laptop into the router with a cable removes one hop. bit-flow detects this case and says so:

```
tip: both computers are on Wi-Fi through the same router, so every byte crosses the air twice.
     Plugging either one into the router with a cable roughly doubles the speed.
```

Other things that help on Wi‑Fi: use the 5 or 6 GHz network (not 2.4 GHz), sit closer to the router, and
avoid a router busy with someone's 4K stream.

### Cables

Any cable that gives the two computers network addresses works with no setup:

- **Ethernet**: directly between the laptops, or both into the same switch or router.
- **Thunderbolt / USB4** between two Macs (or PCs with USB4 networking): macOS sets up a "Thunderbolt
  Bridge" automatically. It's the fastest option by far.
- **USB-C to Ethernet adapters** work the same as built-in Ethernet.

`bitflow ifaces` shows which links each computer has.

## 2. Multipath: Wi‑Fi and cable at once

When both computers are on more than one link (say Wi‑Fi and a Thunderbolt cable), bit-flow spreads its
streams across all of them. Each stream pulls work from one shared queue, so the faster link automatically
carries more. Extra links are tested quickly before use (anything that doesn't answer within 700 ms is left out),
and VPN or virtual interfaces are ignored. The transfer shows `via wi-fi + thunderbolt bridge`.

## 3. Compression

bit-flow compresses each 4 MiB block with zstd (fastest level) and sends it compressed only if that saves
at least 10%.

- **Games and apps** compress well: executables, many asset formats and logs. Measured on Chrome.app
  (1.5 GB), 49.5% of the bytes never had to cross the link. On a Wi‑Fi-limited transfer that's close to 2×
  faster.
- **Already-compressed files** (video, photos, zip/7z, most media) are recognized by extension and skipped.
  A file whose first blocks don't shrink is sent raw from then on. Testing a block costs almost nothing:
  incompressible data is rejected at about 9 GB/s.
- **On very fast links** (Thunderbolt) compression would become the bottleneck, so `auto` mode turns
  itself off when the link outruns it. It also pauses for 20 s when a 64 MB stretch saves under 5%.

Force it with `--compress on` or `--compress off`.

## 4. Parallel streams

One TCP connection rarely fills a fast Wi‑Fi link, because of loss recovery and how Wi‑Fi batches packets.
bit-flow opens 16 encrypted connections and starts with 4 carrying data. Every 1.5 s it measures throughput,
doubles the active count while that buys at least 8% more, then settles on the best count. `--streams N`
fixes the count instead.

## 5. Big files and small files

- **Big files** are split into 32 MiB chunks taken from a shared queue, largest files first, so the
  transfer never ends waiting on one huge file.
- **Small files** (≤ 1 MiB) travel in packs of up to 4 MiB, and the receiver creates them 16 at a time.
  Games ship tens of thousands of small files, and handling them one by one would be the bottleneck.
- **Skipped entirely**: files the receiver already has (same size and time), and the parts of files that
  an interrupted transfer already delivered.

## 6. Disks

At multi-GB/s, disks become the limit. A laptop SSD typically writes 2–3 GB/s, and an external hard drive
about 150 MB/s. Windows Defender scanning every new file slows down transfers with many small files a lot.
Temporarily excluding the destination folder from real-time scanning helps.

## Measure your setup

Start `bitflow recv` on one laptop, then on the other:

```bash
bitflow bench --to <name> --code <code> --sweep     # link + engine, no disks, 1–32 streams
```

```
Benchmark → 192.168.1.40:7700: 10.00 GB per run, block 4MiB, TLS, checksums on
   1 streams   10.00 GB in 2m31s  —  66.2 MB/s (530 Mbit/s)
   ...
```

Compare it with a real `send` of your game folder: if the send is much slower than the bench, the disks or
small files are the limit. If the bench itself is slow, it's the link, so try the tips above.

`bitflow bench --loopback --sweep` measures the engine alone on one machine. On an Apple M4 it does
5–6 GB/s encrypted and about 9 GB/s raw, far above any Wi‑Fi.

## Raw mode for trusted cables

On a direct cable between your own two laptops you can skip encryption on the data streams:

```bash
bitflow recv --to ~/Games --allow-raw        # receiver opts in
bitflow send ~/Games/Big --to other --raw    # sender asks
```

Pairing still happens, and each data stream still has to prove it belongs to the session, but the data
travels unencrypted. On modern CPUs, encryption only matters above about 2–3 GB/s per stream, so this is
rarely worth it outside Thunderbolt links.
