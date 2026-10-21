<div align="center">

# bit-flow

**Move huge files between the devices in your room, as fast as the link allows.**

A 40 GB game, a folder of 4K video, a whole drive's worth of photos: laptop to laptop, phone to laptop,
phone to iPad. No USB stick, no cloud upload, no accounts, no internet.

<img src="docs/img/app.jpg" alt="bit-flow sending a 12.9 GB game" width="760">

[**Download**](https://github.com/sshivamanand/bit-flow/releases) ·
[Get started](docs/getting-started.md) ·
[Phones & the room](docs/phones.md) ·
[Speed guide](docs/speed.md) ·
[All guides](docs/README.md)

</div>

---

## Why

Cloud uploads crawl. USB sticks are never big enough. AirDrop only works between Apple devices, and most
"send to a friend" apps choke on 100 GB.

bit-flow is one small program. Run it on both computers, pick what to send, type the 6-digit code shown on
the other screen, and it goes **straight from one machine to the other** over your Wi‑Fi or a cable. It's
built for the big stuff: it uses several connections at once, survives Wi‑Fi drops, and picks up where it
left off.

## Highlights

|  |  |
|---|---|
| ⚡ **Fast** | Several encrypted connections at once, compression when the link is the bottleneck, and every route between the two machines (Wi‑Fi *and* Ethernet *and* Thunderbolt) used together. On a cable, 100 GB takes minutes. |
| 🔁 **Never starts over** | Wi‑Fi dropped? It reconnects by itself. Closed the laptop halfway? Send the same thing again and only the missing part moves. Files the other side already has are skipped. |
| 📱 **The room** | Any phone, tablet or laptop on the Wi‑Fi opens one link and joins. Each device sees the others and can send to any of them, with nothing to install. |
| 🎮 **Steam games** | Pick a game and it lands straight in your friend's Steam library, ready to play. Updates only send the files that changed. |
| 🔒 **Safe by default** | A pairing code guards every transfer and everything between computers is encrypted. Nothing is saved until the receiver says yes, and nothing is overwritten unless they choose to. |
| 🧳 **One file, no setup** | A single program for Mac, Windows and Linux. No installer, no account, no server, no tracking. A friend without bit-flow can download it from *your* laptop, no internet needed. |

## Get going in 5 minutes

**You need** two computers (Mac, Windows or Linux) on the same Wi‑Fi or router.

**1. Download it on both.** Grab the file for your computer from the
**[Releases page](https://github.com/sshivamanand/bit-flow/releases)** and unzip it:

| Your computer | Download |
|---|---|
| Mac with Apple silicon (M1 and newer) | `…darwin_arm64.tar.gz` |
| Intel Mac | `…darwin_amd64.tar.gz` |
| Windows PC | `…windows_amd64.zip` |
| Linux | `…linux_amd64.tar.gz` |

Inside is a single file, `bitflow` (`bitflow.exe` on Windows). That's the whole program.

**2. Start it.** Open a terminal (*Terminal* on Mac, *PowerShell* on Windows) in that folder and run:

```bash
./bitflow          # Mac / Linux
```

```powershell
.\bitflow.exe      # Windows
```

A browser tab opens. The first time, it asks what other devices should call you. Type a name like
`maya-laptop` and press Enter.

**3. Send.** The other laptop shows up under **nearby**. Click **files**, **folder** or **game**, type the
**6-digit code shown on the other laptop's screen**, and they click **accept**. You'll see the speed and
time left.

<div align="center"><img src="docs/img/offer.jpg" alt="The receiver's accept prompt for a 40.8 GB game" width="560"></div>

<details>
<summary><b>Your computer shows a warning the first time?</b></summary>

That's normal for small programs that don't come from a big company.

- **Mac:** if it says it "can't be opened", run `xattr -d com.apple.quarantine bitflow` in Terminal and try
  again. When asked about incoming network connections, click **Allow**.
- **Windows:** on the blue "Windows protected your PC" box, click **More info → Run anyway**. If the
  firewall asks, tick **Private networks** and click **Allow**.
- **Linux:** usually nothing. If you run a firewall, allow port 7700.
</details>

> **Friend doesn't have bit-flow?** While yours is running, they can download it from your laptop with no
> internet: press `p` in the app and they open the address shown. See [Phones & browsers](docs/phones.md).

## The room: phones, tablets, any browser

<img src="docs/img/room.jpg" alt="The room on a phone, with an incoming transfer" width="260" align="right">

The app shows an invite address like `http://192.168.1.40:7700` under **nearby** (**show qr** gives phones a
code to scan). Every device that opens it joins **the room**:

- Phones and browsers show up in your **nearby** list, right next to bit-flow computers. Send to them like
  anything else, and they can send to you: you get the normal accept prompt and files land in your folder.
- Everyone in the room sees everyone else in a live list.
- Each device has its own 6-digit code on its screen. To send, tap a device, pick files, and type the code
  shown on *that* device.
- The receiver taps **accept & download**. Several files arrive as one zip.
- After that, the two can send back and forth with no code.

Only the computer hosting the room needs bit-flow. Phone-to-phone files pass through it on the way and are
never stored there. bit-flow computers always use their own direct, encrypted path, never the browser lane.
[More about the room](docs/phones.md)

<br clear="right">

## How fast?

Your Wi‑Fi is almost always the limit, not bit-flow. Rough times for **100 GB**:

| Connection | Time |
|---|---:|
| Both laptops on Wi‑Fi, same router | 20–50 min |
| One laptop plugged into the router | 11–17 min |
| Ethernet cable between them | about 15 min |
| Thunderbolt / USB4 cable (Macs) | 1–2 min |

The two easiest wins:

1. **Plug one laptop into the router.** When both use Wi‑Fi, every byte crosses the air twice. One cable
   roughly doubles the speed, and bit-flow tells you when that applies.
2. **Connect the laptops directly** with Ethernet or Thunderbolt if you can. bit-flow uses the cable *and*
   the Wi‑Fi together.

Measured on an Apple M4: the engine alone moves 6–15 GB/s, so the network is always what's holding it back.
More in the [speed guide](docs/speed.md).

## Prefer the terminal?

Everything the app does also works from the command line:

```bash
# receiving laptop: prints a pairing code and its name
bitflow recv --to ~/Downloads

# sending laptop: finds the receiver by itself
bitflow send ~/Games/BigGame --code 482913

# send a Steam game into the other laptop's Steam library
bitflow send --steam "Baldur's Gate 3" --code 482913

# past transfers (CLI, app and phone uploads)
bitflow history
```

Add `--to their-name` (or an IP address) when there are several receivers. Every command and flag is in the
[command reference](docs/cli.md).

## Not finding the other laptop?

- bit-flow has to be **running on both**.
- Both must be on the **same network**. Guest, hotel and office Wi‑Fi often stop devices from seeing each
  other. A phone hotspot is a good workaround.
- Type the other laptop's address (like `192.168.1.40`) into the **or** box. Its screen shows it at the
  bottom.
- Still stuck? See [Troubleshooting](docs/troubleshooting.md).

## Good to know

- **Local network only.** It never goes over the internet, and nothing is sent to any server.
- **One transfer at a time** per laptop. If it's busy, the sender gets a clear message.
- **Received files** go to `Downloads/bit-flow` unless you pick another folder in **settings**.
- **History stays tidy.** A phone upload of 300 photos is one line, and you can expand it to see every file.
  Remove one line with **×**, or wipe it all with **clear history**.
- **Browser traffic in the room isn't encrypted** (plain HTTP on your Wi‑Fi). That's fine at home. On shared
  Wi‑Fi, use the app on both computers for anything private. [Security details](docs/security.md)

## Guides

| | |
|---|---|
| [Getting started](docs/getting-started.md) | Install and your first transfer, step by step |
| [The app](docs/app.md) | Every screen, button and keyboard shortcut |
| [Phones & browsers](docs/phones.md) | The room, phones, and giving bit-flow to a friend |
| [Steam games](docs/games.md) | Sending games and updates |
| [Speed guide](docs/speed.md) | Getting the most out of your setup |
| [Command line](docs/cli.md) | Every command and option |
| [Troubleshooting](docs/troubleshooting.md) | When something doesn't work |
| [Security](docs/security.md) | What's protected, and how |

## Under the hood

bit-flow is a single Go program with no cgo. The two laptops turn the 6-digit code into shared keys with a
PAKE (CPace), so someone else on the network can't pair even if they watch the traffic. Then they open
several TLS 1.3 connections pinned to those keys. Files are split into 32 MB pieces (small files are packed
together) that all connections pull from. Every piece is checked with XXH3 before it's written, and a
journal tracks what's safely on disk. That journal is what makes resume work.

<img src="docs/architecture.png" alt="Architecture diagram" width="100%">

For the full story: [ARCHITECTURE.md](ARCHITECTURE.md) (design), [docs/protocol.md](docs/protocol.md) (wire
format), [docs/development.md](docs/development.md) (building and testing) and [PLAN.md](PLAN.md)
(milestones and measured results).

**Building from source:** clone the repo and run `./bitflow` from its top folder. It builds itself the first
time (needs [Go](https://go.dev/dl/)). Or use `make build`, `make test` and `make cross`.

## Status

Built and tested on macOS, with automated tests on macOS, Linux and Windows on every change. It hasn't been
benchmarked between two separate laptops or tried on real Windows hardware yet, so treat the speeds above as
estimates. See [PLAN.md](PLAN.md) for what's next.

<div align="center"><sub>Made by Shivam Anand.</sub></div>
