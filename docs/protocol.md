# Wire protocol

This describes protocol **version 4** (`proto.Version`), as implemented in `internal/proto`,
`internal/secure` and `internal/transfer`. Integers are big-endian.

## Connections

A transfer uses one **control** connection and N **data** connections to the same TCP port. The side
that *listens* is usually the receiver, but in `send --wait` / `recv --from` mode it's the sender. Session
roles (who sends files) are independent of who dials. The dialer is always the PAKE "dialer" and the TLS client.

Browsers share the port: a connection whose first four bytes are an HTTP method (`GET `, `POST`, `PUT `, …)
goes to the web server, and `BFLW` goes to the protocol below (`internal/web.Split`). The browser room
is described [at the end](#the-room-browsers).

## Hello (24 bytes, every connection)

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | magic `BFLW` |
| 4 | 1 | version (4) |
| 5 | 1 | kind: `1` control, `2` data (TLS), `3` data (raw), `4` probe |
| 6 | 16 | session ID (random, chosen by the dialer) |
| 22 | 2 | stream index |

A **probe** (kind 4) gets one control-style message `Info` (type 6, the device's discovery JSON) and is closed.

## Pairing (control connection, right after Hello)

Using CPace (ristretto255), context `idA="bitflow-dialer"`, `idB="bitflow-listener"`,
`ad="bitflow/v2 pairing " ‖ sessionID`. Blobs are `u16 length ‖ bytes`.

```
dialer   → listener : blob(msgA)
listener → dialer   : blob(msgB) ‖ fpL[32] ‖ macL[32]
dialer   → listener : fpD[32] ‖ macD[32]
```

- `fpX` = SHA-256 of that side's ephemeral self-signed certificate (ECDSA P-256).
- `K` = the CPace key. `confirm(role) = HKDF-Expand-SHA256(K, "bitflow/v2 confirm " ‖ role, 32)`.
- `transcript = sessionID ‖ blob(msgA) ‖ blob(msgB) ‖ blob(fpL)`
- `macL = HMAC-SHA256(confirm("listener"), transcript)`
- `macD = HMAC-SHA256(confirm("dialer"), transcript ‖ fpD)`

A MAC mismatch means the codes differ (`ErrWrongCode`). The listener counts it and sleeps 1 s, and after 5
in a row it picks a new code.

Then the connection becomes **TLS 1.3**, with the dialer as client. Both sides present their certificates and
verify the peer's SHA-256 fingerprint against the one from pairing (no CA chain).

## Control messages

`u32 length ‖ u8 type ‖ JSON body` (length counts the JSON only, max 512 MiB).

| Type | Name | Direction | Body |
|---:|---|---|---|
| 1 | Offer | sender → receiver | `from`, `files[]`, `total`, `streams`, `block`, `bench?`, `nohash?`, `raw?`, `addrs[]`, `steam?` |
| 2 | Accept | receiver → sender | `ok`, `reason?`, `raw?`, `dest?`, `addrs[]`, `skip[]`, `have[]`, `already` |
| 3 | Progress | receiver → sender | `bytes`: held by the receiver, including `already` (every 250 ms) |
| 4 | Complete | receiver → sender | `bytes`, `already`, `files`, `elapsed_ns` |
| 5 | Error | either | `message`, `fatal?` (fatal: retrying the same offer can't succeed) |
| 6 | Info | probe reply | discovery info |

`files[]` entries: `{"p": path, "s": size, "m": mode, "t": mtime_ns, "d": isDir, "l": linkTarget}`. Paths
are relative, `/`-separated, and start with a top-level name the sender picked. `have[]`:
`{"f": fileIndex, "r": [{"o": offset, "l": length}, …]}`. `steam`: `{appid, name, installdir, os}`.

### Session flow

```
sender                                   receiver
  Offer  ─────────────────────────────▶   validate, check conflicts + free space,
                                          load resume journal, ask the user
         ◀─────────────────────────────  Accept (skip / have / raw / addrs)
  open N data connections (TLS, or raw + token), possibly over several links
  frames on all streams  ─────────────▶   verify, decompress, write at offset
         ◀─────────────────────────────  Progress …
  END frame on every stream ──────────▶
         ◀─────────────────────────────  Complete (or Error)
```

## Data connections

After Hello, a **TLS** data stream (kind 2) does a TLS 1.3 handshake pinned to the same fingerprints. A
**raw** stream (kind 3) instead sends a 32-byte token,
`HMAC-SHA256(HKDF-Expand(K, "bitflow/v2 raw stream", 32), sessionID ‖ u16 stream)`. It's only accepted
when the receiver allowed raw mode.

Then frames follow back to back:

### Frame header (37 bytes)

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | file index (into `Offer.files`) |
| 4 | 8 | offset in the file |
| 12 | 4 | `len`: payload bytes on the wire |
| 16 | 4 | `rawLen`: payload bytes after decompression |
| 20 | 1 | flags: `1` END, `2` PACK, `4` COMPRESSED |
| 21 | 16 | XXH3-128 of the **raw** payload (zero with `nohash`) |

- **END**: no payload. The stream is finished.
- **COMPRESSED**: the payload is a zstd frame. `rawLen ≤ block size` and must equal the decompressed length.
- **PACK**: the raw payload is a sequence of whole small files, each `u32 fileIndex ‖ u32 size ‖ bytes`.
  File index and offset in the header are zero.

Blocks are at most `Offer.block` bytes (default 4 MiB). A chunk (default 32 MiB) is the scheduling unit, sent
as consecutive blocks on one stream. Different chunks of the same file travel on different streams in parallel.

## Discovery (UDP 7700)

Query (to `224.0.0.177:7700` on every interface, to each subnet's broadcast, and to `255.255.255.255`, sent 3×):

```json
{"bitflow": 4, "q": true, "from": "<device id>"}
```

Answer (unicast back to the asker):

```json
{"bitflow": 4, "id": "…", "name": "gaming-rig", "os": "darwin", "port": 7700, "role": "recv", "offer": "…"}
```

`role` is `recv` (accepting files) or `send` (waiting to be fetched). `port` is the TCP port. If nothing
answers, scanners probe every address of each local /24 over TCP with a Hello of kind 4.

## Resume journal (receiver, not on the wire)

`<cache>/bitflow/resume/<key>.json` holds the byte ranges known to be on disk for each partial file,
written as snapshot → fsync → atomic rename every 3 s and when a transfer stops. The key hashes the
destination plus each **file's** path, size, mtime and link target. Directories are excluded because their
mtimes change on their own.

## The room (browsers)

Browsers don't speak the protocol above. The room (`internal/web/room.go`) is plain HTTP on the same port.

**WebSocket `GET /room/ws`** (same-`Origin` only) carries presence, pairing and progress as JSON objects with
a `type` field. No file data goes over it.

| From | Type | Fields | Meaning |
|---|---|---|---|
| page | `hello` | `name`, `key`, `kind` | first message; `key` from an earlier `welcome` rejoins the same seat (30 s grace) |
| server | `welcome` | `id`, `key`, `code`, `name`, `kind` | your identity, secret key and 6-digit code |
| server | `roster` | `members[]`: `id`, `name`, `kind`, `paired`, `online` | everyone else; `paired` = you can send without a code |
| page | `rename` | `name` | server answers `name` (it may add a number to keep names unique) |
| page | `offer` | `to`, `code`, `files[]`: `name`, `size` | ask to send; `code` is ignored once paired |
| server | `offered` / `offer-error` | `xfer`, `to` / `to`, `error` | to the sender |
| server | `incoming` | `xfer`, `from`, `fromName`, `files`, `total` | to the receiver |
| server | `code` | `code` | your code changed after 5 wrong guesses |
| server | `accepted`, `progress`, `done`, `declined`, `cancelled`, `failed` | `xfer`, `bytes`, `total`, `error` | to both sides |
| page | `decline`, `cancel` | `xfer` | |
| page / server | `ping` / `pong` | | every 25 s; a socket idle for 75 s is dropped |

**Data.** Both requests carry the tab's key as `?k=`.

- `GET /room/dl/{xfer}`: the receiver's download, and opening it is what accepts the offer. One file is sent
  as itself (`Content-Length` set); several are a store-only zip built on the fly. Only one download per transfer.
- `PUT /room/up/{xfer}/{i}` (needs `X-Bitflow: 1`): the sender uploads file `i`, in order, after `accepted`.
  The server copies the body straight into the download, never more than the offered size, and answers when
  that file is through. A cancel on either side cuts both requests.

`GET /room/qr` is an SVG QR code of the page's own address.

