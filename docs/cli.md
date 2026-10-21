# Command line

```
bitflow                     open the app
bitflow ui                  open the app (with options)
bitflow recv                receive files
bitflow send                send files, folders, or a Steam game
bitflow peers               list bit-flow devices on the network
bitflow games               list installed Steam games
bitflow history             past transfers
bitflow config              show or change settings
bitflow bench               measure a link or the engine
bitflow ifaces              this computer's network links
bitflow version
```

Flags can go before or after paths (`bitflow send ./Game --to desk` works). Sizes accept `4MiB`, `10GB`,
`1.5g` and so on. Whoever *waits* shows a pairing code, and the other side passes it with `--code` (or is
asked for it).

---

## `bitflow recv`

Wait for transfers and save them into a folder.

```bash
bitflow recv --to ~/Downloads
bitflow recv --to ~/Games --yes --on-conflict update
bitflow recv --to ~/Downloads --from shivam --code 482913    # fetch from a waiting sender
```

| Flag | Default | |
|---|---|---|
| `--to DIR` | configured folder, else `.` | where files are saved (must exist) |
| `--yes` | off | accept without asking |
| `--once` | off | exit after one transfer |
| `--code CODE` | random | use a fixed pairing code (6+ digits); with `--from`, the sender's code |
| `--on-conflict POLICY` | configured, else `error` | what to do when a *different* file with the same name exists: `error` refuses the transfer, `skip` keeps yours, `rename` saves theirs as `Name (1)`, `update` replaces only files that changed, `overwrite` replaces |
| `--overwrite` | | shorthand for `--on-conflict overwrite` |
| `--allow-raw` | off | let senders use `--raw` (unencrypted data, for trusted direct cables) |
| `--no-resume` | off | delete partial files of interrupted transfers instead of keeping them for resume |
| `--steam-library PATH` | main library | where incoming Steam games go; `-` saves them as plain folders |
| `--from NAME\|HOST` | | fetch from a sender running `send --wait` |
| `--retries N` | 10 | with `--from`: reconnect attempts after a dropped connection |
| `--port N` | 7700 | TCP port (and the advertised port for discovery) |
| `--sockbuf SIZE` | OS autotuning | socket buffer size |

While waiting, `recv` also answers phones and browsers on the same port: they can upload files after
entering the pairing code, and download bit-flow itself. See [Phones & browsers](phones.md).

## `bitflow send`

```bash
bitflow send ~/Games/BigGame --to gaming-rig --code 482913
bitflow send movie.mkv photos/ --to 192.168.1.40:7711
bitflow send ~/Games/BigGame                     # find the receiver automatically
bitflow send ~/Games/BigGame --wait              # wait for the receiver to fetch
bitflow send --steam "elden ring" --to gaming-rig
```

| Flag | Default | |
|---|---|---|
| `--to NAME\|HOST[:PORT]` | discover | receiver by device name (from discovery) or address. Without it, bit-flow picks the only receiver it finds, asks if there are several, and waits to be fetched if there are none. |
| `--code CODE` | asked | the receiver's pairing code |
| `--wait` | off | don't dial: show a code and wait for `recv --from` (useful when the receiver can't accept connections). Phones can download what you offer in a browser. |
| `--steam GAME` | | send an installed Steam game (name or app ID) into the receiver's Steam library |
| `--streams N` | auto | parallel connections; auto opens 16 and grows the active count 4 → 8 → 16 while it helps |
| `--compress MODE` | `auto` | zstd: `auto` stays on while it pays off, `on`, `off` |
| `--raw` | off | unencrypted data streams (receiver needs `--allow-raw`) |
| `--retries N` | 10 | reconnect attempts after a dropped connection (only once a transfer has started) |
| `--chunk SIZE` | 32MiB | work unit: big files are split into chunks that streams pull from a shared queue |
| `--block SIZE` | 4MiB | frame size on the wire (4KiB–64MiB) |
| `--nohash` | off | skip per-block checksums |
| `--port N` | 7700 | with `--wait`: port to listen on |
| `--sockbuf SIZE` | OS autotuning | socket buffer size |

What gets sent: regular files, folders (including empty ones), and symlinks that point inside what you're
sending. Permissions and modification times are kept. `.DS_Store`, `Thumbs.db` and `desktop.ini` are left
out. Devices, sockets, and symlinks pointing elsewhere are skipped with a note.

Once accepted, `send` prints the link in use and any advice, for example:

```
  link: Wi-Fi → Wi-Fi
  tip: both computers are on Wi-Fi through the same router, so every byte crosses the air twice.
       Plugging either one into the router with a cable roughly doubles the speed.
  4.16 GB / 12.88 GB   32.3%  [███████·················]  112.4 MB/s  ETA 1m17s  · 8 streams · zstd saved 1.2 GB
```

## `bitflow peers`

List bit-flow devices on the network: name, OS, role, addresses, and how each was found.

```bash
bitflow peers
bitflow peers --wait 3s
```

## `bitflow games`

List installed Steam games in every Steam library on this computer. See [Steam games](games.md).

## `bitflow history`

```bash
bitflow history          # last 20
bitflow history -n 100
```

Every transfer from the command line, the app, and phone uploads, newest first, with size, average speed,
and outcome.

## `bitflow config`

```bash
bitflow config                          # show settings
bitflow config name "gaming rig"
bitflow config dest ~/Games
bitflow config on_conflict update
```

| Key | |
|---|---|
| `name` | device name others see (1–40 characters) |
| `dest` | default folder for `recv` and the app |
| `on_conflict` | default policy: `error`, `skip`, `rename`, `update`, `overwrite` |

## `bitflow bench`

Measure without touching the disk: synthetic data is sent, verified and discarded.

```bash
bitflow bench --loopback --sweep                     # this machine's engine ceiling
bitflow bench --to gaming-rig --code 482913 --sweep  # the real link to another laptop
```

| Flag | Default | |
|---|---|---|
| `--loopback` | | benchmark against an in-process receiver |
| `--to NAME\|HOST` | | benchmark against a running `bitflow recv` |
| `--code CODE` | asked | that receiver's code |
| `--size SIZE` | 10GB | bytes per run |
| `--sweep` | off | try 1, 2, 4, 8, 16 and 32 streams |
| `--raw`, `--streams`, `--compress`, `--chunk`, `--block`, `--nohash`, `--sockbuf` | | as for `send` (compression defaults to off for benchmarks) |

## `bitflow ifaces`

Show this computer's network links with friendly names (Wi‑Fi, Ethernet, Thunderbolt Bridge) and addresses.

## Environment variables

| Variable | |
|---|---|
| `BITFLOW_CONFIG_DIR` | use another config folder (settings, sessions, history), e.g. to run two copies on one machine |
| `BITFLOW_STEAM_ROOT` | look for Steam here instead of the usual locations (separate several with `:`, or `;` on Windows) |

## Exit codes

`0` success · `1` error, including bad flags (message on stderr) · `2` unknown command · `130` cancelled with Ctrl+C
