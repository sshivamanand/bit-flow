# Troubleshooting

## The other computer doesn't show up under "nearby"

1. **Is bit-flow running there?** It must be in the app (`bitflow`) or `bitflow recv`. A plain `send` isn't
   discoverable. `send --wait` is, but as a sender.
2. **Same network?** Both computers must be on the same Wi‑Fi or wired network. A phone hotspot works too.
3. **Firewall.** Allow bit-flow when macOS or Windows asks (on Windows, for *Private* networks). On Linux,
   open TCP and UDP port 7700.
4. **Guest and hotel Wi‑Fi** often block devices from talking to each other ("client isolation").
   Discovery falls back to probing the local network directly, but if that's blocked too, nothing on that
   network will work. Use a phone hotspot, or a cable between the laptops.
5. **Type the address instead.** The receiver prints its addresses (`bitflow ifaces` shows them too), so
   enter one under **or [address]** in the app, or use `--to 192.168.x.y`.

## "found X, but couldn't connect to it"

Discovery reached the other computer but a direct connection didn't work. Usually that's client isolation
or a firewall on the receiving side. Try swapping roles: the side that *can't* accept connections should
dial out.

```bash
bitflow send <stuff> --wait                     # on the computer with the files
bitflow recv --to <folder> --from <name>        # on the other one
```

## "wrong pairing code"

The code shown on the other screen changes after 5 wrong tries, and every time bit-flow restarts (unless
you set `--code`). Check the current code on that screen.

## "… already exist at the destination and differ"

A different file with the same name is already in the destination. The receiver chooses what happens:
`--on-conflict rename` (keep both), `skip` (keep yours, skip theirs), `update` (replace changed files, skip identical
ones), or `overwrite`. In the app, it's under **settings**. To set a default: `bitflow config on_conflict update`.

## It's slower than I expected

Start with the [speed guide](speed.md). The usual suspects:

- **Both laptops on Wi‑Fi through one router**: plug one in. bit-flow shows a tip when this is the case.
- **2.4 GHz Wi‑Fi**: switch to the 5 or 6 GHz network.
- **Lots of tiny files on Windows**: Defender scans each new file. Exclude the destination folder while
  transferring.
- **External hard drive**: about 150 MB/s is its limit.

`bitflow bench --to <name> --sweep` separates link speed from disk speed.

## The transfer stopped halfway

Wi‑Fi drops are retried automatically (10 times, waiting up to 10 s between tries). If the sender gave up
or was closed, **run the same send again**: the receiver keeps the partial files (`*.bitflow-partial`)
plus a record of which bytes are safely written, and only the rest is sent. To discard an interrupted
transfer instead, delete the `.bitflow-partial` files (or receive with `--no-resume`).

## The app says "bit-flow isn't running anymore"

The page lost contact with the agent. Start `bitflow` again and reload the page. The browser stays signed in.

## The app page says "this browser isn't signed in"

Open the link bit-flow printed when it started (it contains a one-time token). After that, the bookmark
`http://127.0.0.1:7710` works on its own.

## A Steam game doesn't appear after receiving it

Restart Steam: it only reads new `appmanifest` files when it starts. If the receiver has no Steam
installation, the game was saved as a plain folder. See [Steam games](games.md).

## macOS says bitflow "can't be opened"

It was downloaded in a browser and quarantined. Run `xattr -d com.apple.quarantine /path/to/bitflow`
once, or install with `curl … | sh`, which doesn't set the flag.

## Copied symlinks are missing on Windows

Windows only lets programs create symlinks with Developer Mode turned on (or as administrator). Everything
else in the transfer arrives normally. Turn on Developer Mode in Settings → For developers, then send again.

## FAQ

**Does it need the internet?** No. Everything happens on your local network, including getting bit-flow
onto a friend's laptop.

**Is there a size limit?** No practical one. Files are streamed and written in place from a fixed pool of
buffers, so memory use stays bounded (a few hundred MB at most, at 16 active streams) whatever the size.

**Can several people send to me at once?** One transfer at a time. Others are told the receiver is busy.

**Where do received files go?** The folder you chose (`--to`, or **settings → save received files in**),
keeping the sender's folder structure.

**Where are my settings and history?** See [the config and cache folders](README.md#at-a-glance).
