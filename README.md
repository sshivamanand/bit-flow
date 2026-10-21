<div align="center">

# bit-flow

**Send huge files to another laptop, fast.**<br>
Games, videos, whole folders. 100 GB is fine. No USB drive, no internet, no accounts.

<img src="docs/img/app.jpg" alt="bit-flow sending a 12.9 GB game" width="760">

</div>

---

## What is this?

You and a friend are on the same Wi‑Fi (or plugged into the same router). You want to give them a
10 GB game, a pile of video clips, or a whole folder. bit-flow does that directly from your laptop
to theirs: no cloud upload, no cables to swap, and it keeps going if the Wi‑Fi hiccups.

It's a small program you run on both laptops. It opens a simple page in your browser. You pick what to
send, type a 6-digit number shown on the other laptop, and it goes.

## 5-minute start

**You need:** two computers (Mac, Windows or Linux) on the same Wi‑Fi or network.

### 1. Get bit-flow on both computers

Go to the **[Releases page](https://github.com/sshivamanand/bit-flow/releases)** and download the file for your computer:

| Your computer | Download |
|---|---|
| Mac (2020 or newer, M1/M2/M3/M4) | `…darwin_arm64.tar.gz` |
| Older Mac (Intel) | `…darwin_amd64.tar.gz` |
| Windows PC | `…windows_amd64.zip` |
| Linux | `…linux_amd64.tar.gz` |

Unzip it. Inside is one file called `bitflow` (`bitflow.exe` on Windows). That's the whole program.

> **Friend doesn't have it?** If *you* already have bit-flow running, they can get it from *your*
> computer with no internet. See [Phones & browsers](docs/phones.md).

### 2. Start it

Open a terminal (on Mac: the *Terminal* app; on Windows: *PowerShell*), go to the folder with `bitflow`,
and run:

```bash
./bitflow          # Mac / Linux
.\bitflow.exe      # Windows
```

A browser tab opens. The first time, it asks **what other devices should call you**. Type a name like
`maya-laptop` and press Enter.

### 3. Send something

1. Do steps 1–2 on the **other** laptop too.
2. On the sending laptop, the other one shows up under **nearby**. Click **files**, **folder** or **game**
   and choose what to send.
3. Type the **6-digit pairing code shown on the other laptop's screen** (top right).
4. On the other laptop, a box pops up: click **accept**. That's it. You'll see a progress bar and the speed.

<details>
<summary><b>The first time you run it, your computer may show a warning</b></summary>

This is normal for small programs that aren't from a big company.

- **Mac:** if it says it "can't be opened", open Terminal and run `xattr -d com.apple.quarantine bitflow`, then try again.
  When asked about "incoming network connections", click **Allow**.
- **Windows:** if you see a blue "Windows protected your PC" box, click **More info → Run anyway**.
  If the firewall asks, tick **Private networks** and click **Allow**.
- **Linux:** nothing usually. If you use a firewall, allow port 7700.
</details>

## If the other laptop doesn't show up

- Make sure bit-flow is **running** on both (the browser page is open).
- Both must be on the **same Wi‑Fi**. Guest and hotel Wi‑Fi often block devices from seeing each other. A phone
  hotspot works as a workaround.
- Under **or**, type the other laptop's address (like `192.168.1.40`). Its screen shows it at the bottom.
- More help: **[Troubleshooting](docs/troubleshooting.md)**.

## Make it faster

The speed limit is almost always your Wi‑Fi, not bit-flow. Two things help a lot:

1. **Plug one laptop into the router with a cable.** If both laptops use Wi‑Fi through the same router, every
   byte travels the air twice. One cable can roughly **double** the speed. bit-flow tells you when this applies.
2. **Use a cable between the laptops** (Ethernet, or Thunderbolt/USB4 on Macs) if you can. That's 10× faster
   than Wi‑Fi or more.

Roughly how long **100 GB** takes:

| Connection | Time |
|---|---:|
| Both laptops on Wi‑Fi (same router) | 20–50 min |
| One laptop plugged into the router | 11–17 min |
| Ethernet cable | about 15 min |
| Thunderbolt cable | 1–2 min |

bit-flow also shrinks files while sending when that helps, and uses several connections at once. More in the **[Speed guide](docs/speed.md)**.

## What it can do

| | |
|---|---|
| **Resume** | Wi‑Fi dropped? It reconnects by itself. Closed the laptop? Send the same thing again later and only the missing part moves. |
| **Skips what they already have** | Identical files aren't sent twice. |
| **Steam games** | Click **game**, pick one, and it goes straight into your friend's Steam library. Restart Steam and it's installed. Updates only send what changed. [How](docs/games.md) |
| **Phones** | Press `p` for a QR code. Any phone, tablet or laptop that opens it joins **the room** and can send to any other, with no install. [How](docs/phones.md) |
| **Safe** | A pairing code protects every transfer, and everything is encrypted. Nothing is saved without the receiver clicking accept, and nothing is overwritten unless they choose to. [Details](docs/security.md) |
| **Four colour themes** | Press `t` in the app. |

> **Cloned the repository instead?** Run `./bitflow` from its top folder. It builds itself the first time (needs [Go](https://go.dev/dl/)).

## Prefer typing commands?

Everything in the app also works from the terminal:

```bash
# On the receiving laptop: shows a pairing code and a device name
bitflow recv --to ~/Downloads

# On the sending laptop
bitflow send ~/Games/BigGame --code 482913
```

`bitflow send` finds the other laptop by itself. Add `--to their-name` if there are several. See the
**[command reference](docs/cli.md)** for everything.

## Good to know

- It only works on a **local network** (same Wi‑Fi or cable), never over the internet.
- **One transfer at a time** per laptop. If it's busy, the sender gets a clear message.
- Received files go to the folder you choose (default: `Downloads/bit-flow`). Change it in **settings**.
- Nothing is sent to any server. There are no accounts and no tracking.

## All the guides

| | |
|---|---|
| [Getting started](docs/getting-started.md) | Install and first transfer, in more detail |
| [The app](docs/app.md) | Every screen, button and keyboard shortcut |
| [Command line](docs/cli.md) | Every command and option |
| [Steam games](docs/games.md) | Sending games and updates |
| [Phones & browsers](docs/phones.md) | The room (any browser to any browser), phones, and giving bit-flow to a friend |
| [Speed guide](docs/speed.md) | Getting the most out of your setup |
| [Troubleshooting](docs/troubleshooting.md) | When something doesn't work |
| [Security](docs/security.md) | How it keeps your files safe |
| [Protocol](docs/protocol.md), [Development](docs/development.md), [Architecture](ARCHITECTURE.md) | For people who want to look under the hood |

## Under the hood (for the curious)

bit-flow is a single Go program. Both laptops agree on encryption keys using the 6-digit code, then open
several encrypted connections and split the files into pieces that all connections pull from. The receiver
checks every piece before saving it and keeps track of what's safely written, which is what makes resume work.
Full design: [ARCHITECTURE.md](ARCHITECTURE.md).

<img src="docs/architecture.png" alt="Architecture diagram" width="100%">

## Status

Built and tested on a Mac, with automated tests on macOS, Linux and Windows. It hasn't yet been measured between
two separate laptops or tried on a real Windows PC, so treat speeds in this README as estimates. See [PLAN.md](PLAN.md).
