# Security

bit-flow is designed to be safe to run on a shared network: a café, a dorm, a friend's Wi‑Fi.

## Pairing: a short code, strong keys

Whoever waits (normally the receiver) shows a 6-digit pairing code. The other side uses it to run
**CPace**, a password-authenticated key exchange (PAKE), over the network:

- Both sides end up with the same strong session key **without the code ever being sent**.
- Someone watching the network learns nothing they could use to guess the code offline.
- An active attacker gets **one guess per connection attempt**. Each wrong guess costs a 1-second
  penalty (attempts are handled one at a time), and after 5 wrong guesses the code changes and the
  new one is shown on screen.

Each side then proves it holds the key by sending a MAC of the whole handshake plus the fingerprint of its
freshly generated TLS certificate. A wrong code is detected right there, on both sides.

## Encryption: TLS 1.3, pinned

After pairing, the control connection and every data connection switch to **TLS 1.3**, with both
certificates pinned to the fingerprints exchanged during pairing. There is no certificate authority to trust
or spoof: a connection from anyone who wasn't part of the pairing fails its handshake. On computers with
hardware AES this costs nothing noticeable (about 2.8 GB/s per stream on an Apple M4).

**Raw mode** (`send --raw` + `recv --allow-raw`) skips encryption on the data streams for maximum
speed on a trusted direct cable. Both sides must opt in, the control connection is still TLS, and each raw
data stream must present an HMAC token derived from the session key, so nobody can inject data.

## What a receiver allows

- **Nothing arrives without approval.** The receiver is asked to accept every transfer (unless it chose
  `--yes`).
- **Files stay inside the chosen folder.** Every path is checked: no absolute paths, no `..`, no names
  Windows reserves. Symlinks must point inside the transfer, are created only after all files are
  written, and nothing is ever written through a link. A malicious sender can't use a link to reach outside
  the folder.
- **Nothing is overwritten by surprise.** A different existing file stops the transfer unless the
  receiver chose a conflict policy. Files are written as `name.bitflow-partial` and renamed only when
  complete and verified.
- **Every block is checked** (XXH3-128 over the original bytes) before it is written. Corruption is
  caught, and TLS authenticates who sent it.
- **Free space is checked** before accepting.

## The app

The app's interface listens on `127.0.0.1` only, so other machines can't reach it. Inside your computer:

- the launch link carries a one-time token. The page trades it for a session cookie (`HttpOnly`,
  `SameSite=Strict`) and removes it from the address bar. Sessions are stored **hashed** on disk.
- requests must name `127.0.0.1` / `localhost` as the host, which blocks DNS-rebinding tricks from other
  websites
- changes need a custom header that other websites can't send, which blocks cross-site requests
- a strict Content-Security-Policy, and no third-party scripts, fonts or CDNs

## Phones and browsers

The browser pages on port 7700 are reachable by anyone on your network:

- **Public:** the landing page, the bit-flow downloads (`/get`), and joining the room.
- **Needs the pairing code:** uploading files to the computer, or downloading what `send --wait` offers.
  Wrong codes count against the same 5-try limit and penalty as computer pairing.
- Uploads go through the same path checks and never overwrite.

**The room** (browser to browser, see [Phones & browsers](phones.md)):

- Anyone on the network can join and see the device list (names and device types). That's the point.
- Sending to a device needs **that device's own code**, shown only on its screen. Every wrong guess waits
  1 s, and 5 wrong guesses give it a new code (and forget who it was paired with). The receiver still has
  to accept each transfer.
- Each tab gets a random 128-bit key from the server over its WebSocket. Uploads and downloads must carry
  it, and only the transfer's sender may upload and only its receiver may download, once.
- The WebSocket only accepts connections whose `Origin` is the page itself, so another website open in
  someone's browser can't join the room as them.
- **The host computer relays the bytes** (nothing is written to its disk) and could read them. You're
  trusting whoever runs the room.
- **Browser traffic is plain HTTP.** Browsers can't use bit-flow's pinned keys, so on a shared network
  someone could watch phone uploads and downloads, including the code typed into the page. Prefer the
  app between computers for anything sensitive. Change the code by restarting `recv`, or run it with
  `--code` to set a new one.

## Threat model in one paragraph

bit-flow protects a transfer from **other people on the same network**: they can't read it, change it,
inject files, or pair without seeing your screen. It doesn't protect against a **malicious peer you
paired with on purpose** sending you files you'd rather not have (you still choose to accept, and files never
leave the chosen folder), or against someone who can **see your screen** and read the code.
Discovery broadcasts your device name and OS to the local network. That's how nearby devices find each other.
