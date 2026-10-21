# Phones & browsers

Every bit-flow computer that is receiving (`bitflow`, `bitflow recv`) or offering files (`bitflow send --wait`)
also answers plain browsers on its transfer port. The terminal prints the address with a QR code, and in
the app you press `p`:

```
Phone, or a computer without bit-flow? Open http://192.168.1.40:7700
  █████████████████████████████
  ██ ▄▄▄▄▄ ██▀▄██▀▀ ▄█ ▄▄▄▄▄ ██
  ...
```

Phones and browsers can do three things there.

## Send files from a phone

<img src="img/phone.jpg" alt="Phone page after sending two files" width="320">

1. Open the address (or scan the QR code) on a phone on the same Wi‑Fi.
2. Enter the computer's 6-digit pairing code.
3. Tap **choose files**. Desktop browsers can also **choose a folder**.

Files are uploaded in 8 MB pieces, four at a time, with automatic retries. They land in the computer's
receive folder. Existing files are never overwritten: a second `IMG_1234.jpg` is saved as
`IMG_1234 (1).jpg`. Uploads abandoned for 15 minutes are cleaned up.

## Download files to a phone

On the computer:

```bash
bitflow send ~/Videos/trip.mov ~/Pictures/Album --wait
```

The phone opens the address, enters the code, and gets a download button for each item. Files support
resuming (HTTP Range requests). Folders download as a `.zip`, streamed directly with no temporary copy.

## Give bit-flow to a laptop that doesn't have it

The same page offers bit-flow itself for every OS this computer has a copy of. No internet needed:

- click the button for your OS on the page, or
- macOS / Linux: `curl -fsSL http://192.168.1.40:7700/get/sh | sh` (installs to `~/.local/bin`; set
  `BITFLOW_DIR` to choose another folder)
- Windows PowerShell: `iwr http://192.168.1.40:7700/get/windows-amd64.exe -OutFile bitflow.exe`

A computer always offers its own build. To offer the others too, put the cross-compiled binaries
(`make cross` → `dist/bitflow-<os>-<arch>`) next to the `bitflow` executable or in a `dist/` folder
beside it.

## Good to know

- **Same network only.** Phones must be on the same Wi‑Fi (or the computer's hotspot).
- **The code protects uploads and downloads.** The landing page and the bit-flow downloads are public on
  your network. Uploading and downloading files needs the pairing code, and wrong guesses count against the
  same limit as computer pairing (5 tries, then the code changes).
- **Browser traffic is not encrypted.** Phone-to-computer HTTP on the LAN is plain text; anyone else on
  the same Wi‑Fi could in principle watch it. Use the bit-flow app between computers for anything sensitive.
  See [Security](security.md).
- **It's the slow lane.** Browsers are capped well below what two bit-flow apps achieve. For big
  transfers between laptops, install bit-flow on both.
