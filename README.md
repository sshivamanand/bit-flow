# bit-flow

Fast laptop-to-laptop file transfer over your local network (Wi‑Fi or cable).
One small binary, no accounts, no cloud. Design: [ARCHITECTURE.md](ARCHITECTURE.md) · roadmap: [PLAN.md](PLAN.md).

## Build

```bash
go install github.com/sshivamanand/bit-flow/cmd/bitflow@latest
# or from a checkout:
make build      # → bin/bitflow
make cross      # → dist/bitflow-{darwin,windows,linux}-{arm64,amd64}
make test
```

## Use

### The app

```bash
bitflow
```

This opens bit-flow in your browser (it runs locally, on `http://127.0.0.1:7710`). The first time, it asks
what other devices should call this one. Other bit-flow computers on the network show up under
**nearby**. Pick files or a folder, type the pairing code shown on the other screen, and go. Incoming
transfers pop up with accept or decline and a choice of folder. Keys: `r` refresh, `t` theme, `s` settings.

### The command line

On the **receiving** laptop:

```bash
bitflow recv --to ~/Games
```

It prints a pairing code and its device name. On the **sending** laptop:

```bash
bitflow send ~/Games/BigGame --to gaming-rig --code 482913    # by device name
bitflow send ~/Games/BigGame --to 192.168.1.40 --code 482913  # or by address
bitflow send ~/Games/BigGame                                  # finds the receiver itself
```

If one laptop's firewall blocks incoming connections, flip who waits:

```bash
bitflow send ~/Games/BigGame --wait                        # prints a code and waits
bitflow recv --to ~/Games --from shivam --code 482913      # on the other laptop
```

`bitflow peers` lists bit-flow devices on the network.

### Steam games

```bash
bitflow games                                   # installed Steam games, all libraries
bitflow send --steam "elden ring" --to gaming-rig
```

The game goes straight into the receiver's Steam library. Restart Steam and it shows as installed. If they
already have an older version, only the changed files are sent. In the app, use **game** next to a device.

### Phones, and friends without bit-flow

Every bit-flow computer also answers in a browser at the address it prints (and shows as a QR code;
press `p` in the app). Phones enter the pairing code and can send files to a receiver, or download what a
`send --wait` computer offers. Laptops get bit-flow itself from the same page, with no internet needed:

```bash
curl -fsSL http://192.168.1.40:7700/get/sh | sh
```

Run `make cross` on your machine first so it can hand out builds for every OS, not just its own.

The receiver is asked to accept (`--yes` skips the prompt). Files keep the sender's folder structure,
permissions and timestamps. Everything is encrypted (TLS 1.3, with keys agreed through the pairing code).

- **Interrupted?** Wi-Fi drops are retried automatically. If the sender gives up or is stopped, run
  the same `send` again and it resumes where it stopped.
- **Already have some of it?** Identical files are skipped. For files that exist but differ, the
  receiver picks `--on-conflict=error` (default), `skip`, `rename` (saves as `Game (1)`) or `overwrite`.
- **Direct cable between two trusted laptops?** `recv --allow-raw` + `send --raw` drops encryption
  on the data streams for maximum speed.
- **Wi-Fi and a cable?** bit-flow uses every link the two computers share at once (multipath), and
  tells you when one change, like plugging one laptop into the router, would make it faster.
- **App and game bundles** keep their symlinks, so copied macOS apps still launch.

### Measure your link

With `bitflow recv` running on one laptop:

```bash
bitflow bench --to 192.168.1.40 --code 482913 --sweep    # tries 1–32 streams, no disk involved
bitflow bench --loopback --sweep                         # this machine only: engine ceiling
```

### Tuning flags (send / bench)

| Flag | Default | Meaning |
|------|---------|---------|
| `--streams` | auto | parallel TCP connections; auto opens 16 and tunes how many carry data (4 → 8 → 16) |
| `--compress` | auto | zstd per block: auto stays on while it pays off, `on`, `off` |
| `--chunk` | 32MiB | work unit; files are split into chunks that streams pull from a shared queue |
| `--block` | 4MiB | frame size on the wire |
| `--raw` | off | unencrypted data streams (receiver needs `--allow-raw`) |
| `--retries` | 10 | reconnect attempts after a dropped connection |
| `--nohash` | off | skip per-block XXH3 checksums |
| `--sockbuf` | OS auto | socket buffer size |

## Status

See [PLAN.md](PLAN.md) for milestones and measured results.
