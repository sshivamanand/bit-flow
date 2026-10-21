# Getting started

## Install

bit-flow is a single executable called `bitflow` (`bitflow.exe` on Windows). There's nothing else to install.

**Option 1: a release download.** Grab the archive for your computer from the repository's
**Releases** page:

| Computer | Archive |
|---|---|
| Mac with Apple Silicon (M1–M4) | `bitflow_<version>_darwin_arm64.tar.gz` |
| Intel Mac | `bitflow_<version>_darwin_amd64.tar.gz` |
| Windows PC | `bitflow_<version>_windows_amd64.zip` (`arm64` for Snapdragon laptops) |
| Linux | `bitflow_<version>_linux_amd64.tar.gz` / `linux_arm64` |

Unpack it and put `bitflow` somewhere on your `PATH` (for example `~/.local/bin` or `/usr/local/bin`).

**Option 2: from a friend's laptop, no internet needed.** If one laptop already runs bit-flow, the other can
get it from there. Open the address it shows (`http://192.168.x.y:7700`, or scan its QR code) and pick your
OS, or in a terminal:

```bash
curl -fsSL http://192.168.1.40:7700/get/sh | sh        # macOS / Linux
```
```powershell
iwr http://192.168.1.40:7700/get/windows-amd64.exe -OutFile bitflow.exe   # Windows
```

Builds for other OSes are only offered if that laptop has them (run `make cross` in the repository to bundle
all six).

**Option 3: with Go.**

```bash
go install github.com/sshivamanand/bit-flow/cmd/bitflow@latest
```

### First-run security prompts

- **macOS**: a binary downloaded *in a browser* gets a quarantine flag. If macOS says it "can't be
  opened", run `xattr -d com.apple.quarantine bitflow` once (the `curl … | sh` installer avoids this).
  The first time bit-flow listens, macOS asks whether to accept incoming connections. Click **Allow**.
- **Windows**: SmartScreen may warn about an unrecognized app. Choose **More info → Run anyway**.
  Windows Firewall then asks about network access. Allow it on **Private networks**.
- **Linux**: if you run a firewall, open TCP and UDP port 7700 on your LAN.

## Your first transfer

### With the app

1. On **both** laptops run `bitflow`. The app opens in the browser.
2. The first time, it asks *what should other devices call you?* Type a name (this becomes the device
   name others see) and press enter.
3. Each laptop shows its **pairing code** at the top right.
4. On the sending laptop, the other one appears under **nearby**. Click **files**, **folder**, or **game**
   (Steam), pick what to send, and type the pairing code shown on the *other* laptop.
5. The receiving laptop shows an **accept** prompt with the size, the destination folder (click **change** to
   pick another), and notes such as *"12 identical files already here"*.
6. Watch it go: progress, speed, time left, active streams, bytes saved by compression.

See [the app guide](app.md) for everything else it can do.

### With the command line

```bash
# Receiving laptop: shows a pairing code, its device name, and its addresses
bitflow recv --to ~/Downloads

# Sending laptop: by device name, by address, or let it find the receiver
bitflow send ~/Games/BigGame --to gaming-rig --code 482913
bitflow send ~/Games/BigGame --to 192.168.1.40 --code 482913
bitflow send ~/Games/BigGame                      # finds the only receiver on the network
```

The receiver is asked to accept (`--yes` skips that). Files land in `--to`, keeping their folder structure,
permissions, timestamps and symlinks.

If one laptop's firewall won't accept incoming connections, swap who waits:

```bash
bitflow send ~/Games/BigGame --wait                      # sender shows a code and waits
bitflow recv --to ~/Games --from shivam --code 482913    # receiver fetches
```

## What to expect

- **Speed** depends on the link far more than on bit-flow. Two laptops on the same Wi‑Fi router share
  the air twice, so plugging *one* of them into the router can double the speed. bit-flow tells you when that's
  the case. See the [speed guide](speed.md).
- **Interruptions are fine.** If Wi‑Fi drops, the sender reconnects automatically (up to 10 times). If you
  stop it, run the same command again later and it continues where it left off.
- **Nothing is overwritten by surprise.** Identical files are skipped. If a *different* file with the same
  name exists, the transfer is refused unless the receiver chose another policy (`rename`, `skip`, `update`,
  `overwrite`).
- **The computer stays awake** while a transfer runs.

Next: [the app](app.md) · [the command line](cli.md) · [Steam games](games.md)
