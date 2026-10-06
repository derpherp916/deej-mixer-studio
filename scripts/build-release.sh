#!/usr/bin/env bash
# One-shot release build on Linux (also used by GitHub Actions):
#   firmware -> simulator tests -> editor bundle -> Go tests -> DeejMixer.exe -> installer + portable zip.
# Needs: go 1.24+, node 20+, gcc-avr avr-libc arduino-core-avr simavr libsimavr-dev libelf-dev
#        binutils-mingw-w64-x86-64 nsis zip unzip
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${VERSION:-$(grep -oP 'var AppVersion = "\K[^"]+' "$ROOT/app/icon.go")}"
DIST="$ROOT/dist"; rm -rf "$DIST"; mkdir -p "$DIST"
echo "== Deej Mixer Studio $VERSION =="

echo "-- firmware"
bash "$ROOT/firmware/build_fw.sh"
cp "$ROOT/firmware/DeejMixer/DeejMixer.ino" "$ROOT/app/firmware/DeejMixer.ino"
cp "$ROOT/firmware/build/DeejMixer.hex" "$ROOT/app/firmware/DeejMixer.hex"

echo "-- firmware simulator tests"
gcc -O2 -o "$ROOT/firmware/build/sim_test" "$ROOT/firmware/sim/sim_test.c" -lsimavr -lelf -lm
gcc -O2 -o "$ROOT/firmware/build/sim_bridge" "$ROOT/firmware/sim/sim_bridge.c" -lsimavr -lelf -lm
"$ROOT/firmware/build/sim_test" "$ROOT/firmware/build/DeejMixer.elf"

echo "-- editor bundle"
(cd "$ROOT/editor" && npm ci --silent && npx esbuild entry.js --bundle --minify --format=iife --target=chrome100 \
  --legal-comments=eof --outfile="$ROOT/app/ui/editor.js")

echo "-- Windows resources"
(cd "$ROOT/app/res" && x86_64-w64-mingw32-windres --preprocessor=cpp --preprocessor-arg=-P app.rc -O coff -o ../rsrc_windows_amd64.syso)

echo "-- app tests"
(cd "$ROOT/app" && go vet . && go test -count=1 . \
  && SIM_BRIDGE="$ROOT/firmware/build/sim_bridge" FW_ELF="$ROOT/firmware/build/DeejMixer.elf" go test -tags sim -run RealFirmware -count=1 .)

echo "-- DeejMixer.exe"
(cd "$ROOT/app" && GOOS=windows GOARCH=amd64 go vet -unsafeptr=false . && \
  GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w -X main.AppVersion=$VERSION" -o "$DIST/DeejMixer.exe" .)

echo "-- payload"
P="$ROOT/installer/payload"; rm -rf "$P"; mkdir -p "$P/tools" "$P/drivers/ch341" "$P/firmware/DeejMixer" "$P/licenses" "$P/third-party-source"
cp "$DIST/DeejMixer.exe" "$P/"
cp "$ROOT/vendor/avrdude/avrdude.exe" "$ROOT/vendor/avrdude/avrdude.conf" "$P/tools/"
cp "$ROOT/vendor/ch341/"* "$P/drivers/ch341/"
cp "$ROOT/firmware/DeejMixer/DeejMixer.ino" "$P/firmware/DeejMixer/"; cp "$ROOT/firmware/build/DeejMixer.hex" "$P/firmware/"
cp "$ROOT/LICENSE" "$ROOT/THIRD-PARTY-NOTICES.md" "$ROOT/vendor/avrdude/LICENSE-avrdude.txt" "$ROOT/vendor/Adafruit-NeoPixel-COPYING" "$P/licenses/"
cp "$ROOT/vendor/avrdude/avrdude-8.0.zip" "$ROOT/vendor/Adafruit-NeoPixel-1.15.5.zip" "$P/third-party-source/"
cp "$ROOT/docs/START-HERE.txt" "$P/"

echo "-- installer"
cp "$ROOT/app/res/app.ico" "$ROOT/installer/app.ico"
(cd "$ROOT/installer" && makensis -V2 -DVERSION="$VERSION" DeejMixer.nsi && mv DeejMixer-Setup.exe "$DIST/DeejMixer-Setup-$VERSION.exe")

echo "-- portable zip"
(cd "$ROOT/installer" && cp -r payload "DeejMixer-$VERSION" && zip -qr9 "$DIST/DeejMixer-$VERSION-portable.zip" "DeejMixer-$VERSION" && rm -rf "DeejMixer-$VERSION")
cp "$ROOT/firmware/build/DeejMixer.hex" "$DIST/DeejMixer-firmware-$VERSION.hex"
(cd "$DIST" && sha256sum * > SHA256SUMS.txt)
echo "== done: $DIST =="; ls -la "$DIST"
