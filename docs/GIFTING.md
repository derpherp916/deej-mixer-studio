# Giving a Deej Mixer as a gift

The goal is that the person you give it to plugs it in, runs one installer, and never has to think about it again.

## Before you wrap it

1. **Flash the latest firmware** from your PC (Firmware Studio → Upload, or Settings → Upload stock firmware).
2. Run **Diagnostics** and make sure every button, knob and LED passes.
3. Put a **link to the installer** in the box: the GitHub *Releases → latest* link, the project site, or a QR code to either. A USB stick with `DeejMixer-Setup.exe` on it works too.

## What happens on their PC

| Step | What they do | What happens automatically |
|---|---|---|
| 1 | Plug the mixer in | Windows usually installs the CH340 USB driver itself through Windows Update. |
| 2 | Double-click `DeejMixer-Setup.exe` and click **Yes** (no wizard pages) | App installed and opened; USB driver added to the Windows driver store; Start-menu entry; starts with Windows. |
| 3 | Nothing | The app finds the mixer, loads the default setup and starts controlling volume. |
| Later | Nothing | About 45 s after each Windows start, and then daily, the app checks GitHub for a new release. A new version produces **one** quiet Windows notification, which follows Focus Assist and is never repeated for the same version. Clicking it opens Settings → Updates. |
| Later | Click **Install update** | It downloads, checks the SHA-256 checksum and (if release signing is on) the signature, then installs silently and restarts. Settings are kept. |
| Later | Click the "firmware update ready" notification | If an update ships newer firmware, the app offers it once and flashes it over USB in about 10 seconds. The Nano's bootloader means a failed flash can always be retried. |

## Why the mixer can't install the software by itself

The Arduino Nano's USB port belongs to its CH340 USB-serial chip, so the Nano can only appear to Windows as a serial port. It can't show up as a drive or carry an installer. Windows has also blocked AutoRun from USB devices since Windows 7, so no USB gadget can start an installer on its own.

The closest you can get is a board with native USB (for example an **RP2040** or **ESP32-S3**). When plugged in, it appears as a small drive called "DEEJ MIXER" containing `Setup.exe` and a quick-start page. It also uses Windows' built-in serial driver (no CH340 driver at all) and can act as a media-key keyboard. That would be a hardware revision; the app and protocol are ready for it.

## Publishing updates (for you)

1. Make changes, then bump `CHANGELOG.md`.
2. `git tag v2.1.0 && git push --tags`.
3. GitHub Actions builds, tests and signs the release. Every installed copy sees it within a day.

Release signing: `scripts/publish-to-github.ps1` creates the key for you. To do it by hand, run `go run ./tools/updsign keygen`, then store the private key as the repository secret `UPDATE_SIGNING_KEY` and the public key as the repository variable `UPDATE_PUBKEY`. Apps built with a public key refuse any unsigned or modified release.
