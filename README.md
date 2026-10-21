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

On the **receiving** laptop:

```bash
bitflow recv --to ~/Games
```

It prints a pairing code and the exact command for the other laptop, one line per network link:

```
  Pairing code:  482-913

On the sending computer, run one of these (pick the fastest link):
  Wi-Fi (en0)                bitflow send <path> --to 192.168.1.40 --code 482913
```

On the **sending** laptop:

```bash
bitflow send ~/Games/BigGame --to 192.168.1.40 --code 482913
```

The receiver is asked to accept (`--yes` skips the prompt). Files land under `--to`, keeping the
sender's folder structure, permissions and timestamps. Everything is encrypted (TLS 1.3, keys agreed
through the pairing code).

- **Interrupted?** Wi-Fi drops are retried automatically. If the sender gives up or is stopped, run
  the same `send` again and it resumes where it stopped.
- **Already have some of it?** Identical files are skipped. For files that exist but differ, the
  receiver picks `--on-conflict=error` (default), `skip`, `rename` (saves as `Game (1)`) or `overwrite`.
- **Direct cable between two trusted laptops?** `recv --allow-raw` + `send --raw` drops encryption
  on the data streams for maximum speed.

### Measure your link

With `bitflow recv` running on one laptop:

```bash
bitflow bench --to 192.168.1.40 --code 482913 --sweep    # tries 1–32 streams, no disk involved
bitflow bench --loopback --sweep                         # this machine only: engine ceiling
```

### Tuning flags (send / bench)

| Flag | Default | Meaning |
|------|---------|---------|
| `--streams` | 8 | parallel TCP connections |
| `--chunk` | 32MiB | work unit; files are split into chunks that streams pull from a shared queue |
| `--block` | 4MiB | frame size on the wire |
| `--raw` | off | unencrypted data streams (receiver needs `--allow-raw`) |
| `--retries` | 10 | reconnect attempts after a dropped connection |
| `--nohash` | off | skip per-block XXH3 checksums |
| `--sockbuf` | OS auto | socket buffer size |

## Status

See [PLAN.md](PLAN.md) for milestones and measured results.
