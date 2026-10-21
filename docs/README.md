# bit-flow documentation

**Using bit-flow**

- [Getting started](getting-started.md): install, first transfer, firewall prompts
- [The app](app.md): the browser-based app, shortcuts, themes, settings, sessions
- [Command line](cli.md): every command and flag
- [Steam games](games.md): send installed games straight into a friend's Steam library
- [Phones & browsers](phones.md): the room (any browser to any browser), phones, and giving bit-flow to a laptop that doesn't have it
- [Speed guide](speed.md): Wi‑Fi physics, cables, multipath, compression, benchmarking
- [Troubleshooting](troubleshooting.md): devices not found, slow transfers, FAQ

**How it works**

- [Security](security.md): pairing, encryption, what a receiver allows, threat model
- [Protocol](protocol.md): the wire format
- [Architecture](../ARCHITECTURE.md): design rationale and diagram
- [Plan & measurements](../PLAN.md): milestones and benchmark results

**Working on it**

- [Development](development.md): build, test, end-to-end checks, releases, code layout

## At a glance

| | |
|---|---|
| Transfer port | TCP **7700** (also serves the browser pages) |
| Discovery | UDP **7700** (multicast group 224.0.0.177 + broadcast) |
| App | `http://127.0.0.1:7710`, on this computer only |
| Settings | `config.json` in the config folder (see below) |
| Resume data | `resume/` in the cache folder (see below) |

| | Config folder | Cache folder |
|---|---|---|
| macOS | `~/Library/Application Support/bitflow` | `~/Library/Caches/bitflow` |
| Linux | `~/.config/bitflow` | `~/.cache/bitflow` |
| Windows | `%AppData%\bitflow` | `%LocalAppData%\bitflow` |

Set `BITFLOW_CONFIG_DIR` to use a different config folder (handy for running two copies on one machine).
