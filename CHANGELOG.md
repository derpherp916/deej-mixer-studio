# Changelog

## 2.0.0 — 2026-10-06
First release of Deej Mixer Studio.
- Native Windows app (WebView2 window plus system tray) in the style of GG and iCUE, with a live device view, an inspector panel and profiles
- Per-app volume for 5 knobs, including a **Steam game** target that follows the most recently launched game
- 6 programmable buttons, each with a press action and a hold action: mute, mic mute, launch, hotkey, media keys
- 7-LED level ring and 4 logo LEDs: logos are at full brightness when on and off when muted; ring colour changes while the mic is muted
- **Firmware Studio**: built-in Arduino code editor with verify, upload and serial monitor
- Diagnostics: live hardware test for every button, knob and LED
- Installer adds the CH340/CH341 driver to the Windows driver store
- Firmware v3: debounced press and release events, checksummed LED commands, PC-silence timeout, and a no-PC wiring check (hold the Opera button while plugging in)
