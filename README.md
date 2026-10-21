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

It prints the exact command to run on the other laptop, one line per network link:

```
On the sending computer, run one of these (pick the fastest link):
  Wi-Fi (en0)                bitflow send <path> --to 192.168.1.40
```

On the **sending** laptop:

```bash
bitflow send ~/Games/BigGame --to 192.168.1.40
```

The receiver is asked to accept (`--yes` skips the prompt). Files land under `--to`, with the
sender's folder structure, permissions, and timestamps preserved. Existing files are never touched
unless the receiver passes `--overwrite`.

### Measure your link

With `bitflow recv` running on one laptop:

```bash
bitflow bench --to 192.168.1.40 --sweep    # tries 1–32 streams, no disk involved
bitflow bench --loopback --sweep           # this machine only: engine ceiling
```

### Tuning flags (send / bench)

| Flag | Default | Meaning |
|------|---------|---------|
| `--streams` | 8 | parallel TCP connections |
| `--chunk` | 32MiB | work unit; files are split into chunks that streams pull from a shared queue |
| `--block` | 4MiB | frame size on the wire |
| `--nohash` | off | skip per-block XXH3 checksums |
| `--sockbuf` | OS auto | socket buffer size |

## Status

M0 + M1 done: multi-stream engine, CLI, benchmarks. Not yet: encryption/pairing, resume,
discovery, UI (see [PLAN.md](PLAN.md)). **M1 is unencrypted and unauthenticated, so only run `recv` on
networks you trust.**
