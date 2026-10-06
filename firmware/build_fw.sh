#!/usr/bin/env bash
# Builds firmware/DeejMixer/DeejMixer.ino for the Arduino Nano (ATmega328P) with avr-gcc, without Arduino IDE.
# Needs: gcc-avr, avr-libc, arduino-core-avr (Debian/Ubuntu packages). Output: firmware/build/DeejMixer.{elf,hex}
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
CORE="${ARDUINO_CORE:-/usr/share/arduino/hardware/arduino/avr}"
NEO="$HERE/build/Adafruit_NeoPixel"
OUT="$HERE/build"
rm -rf "$OUT"/core "$OUT"/lib "$OUT"/DeejMixer.*; mkdir -p "$OUT/core" "$OUT/lib"
if [ ! -d "$NEO" ]; then unzip -q -o "$ROOT/vendor/Adafruit-NeoPixel-1.15.5.zip" -d "$OUT"; fi
FLAGS="-mmcu=atmega328p -DF_CPU=16000000L -DARDUINO=10819 -DARDUINO_AVR_NANO -DARDUINO_ARCH_AVR -Os -g -w -ffunction-sections -fdata-sections -flto"
INC="-I$CORE/cores/arduino -I$CORE/variants/eightanaloginputs -I$NEO"
for f in "$CORE"/cores/arduino/*.c; do avr-gcc -c $FLAGS -std=gnu11 -fno-fat-lto-objects $INC "$f" -o "$OUT/core/$(basename "$f").o"; done
for f in "$CORE"/cores/arduino/*.cpp; do avr-g++ -c $FLAGS -std=gnu++11 -fpermissive -fno-exceptions -fno-threadsafe-statics -fno-fat-lto-objects $INC "$f" -o "$OUT/core/$(basename "$f").o"; done
for f in "$CORE"/cores/arduino/*.S; do avr-gcc -c $FLAGS -x assembler-with-cpp $INC "$f" -o "$OUT/core/$(basename "$f").o"; done
for f in "$NEO"/*.c "$NEO"/*.cpp; do
  [ -e "$f" ] || continue
  case "$f" in *.c) C=avr-gcc; S=-std=gnu11;; *) C=avr-g++; S="-std=gnu++11 -fno-exceptions -fno-threadsafe-statics";; esac
  $C -c $FLAGS $S $INC "$f" -o "$OUT/lib/$(basename "$f").o"
done
{ echo '#include <Arduino.h>'; echo '#line 1 "DeejMixer.ino"'; cat "$HERE/DeejMixer/DeejMixer.ino"; } > "$OUT/DeejMixer.cpp"
avr-g++ -c $FLAGS -std=gnu++11 -fno-exceptions -fno-threadsafe-statics -Wall -Wextra $INC "$OUT/DeejMixer.cpp" -o "$OUT/DeejMixer.o"
avr-gcc-ar rcs "$OUT/core.a" "$OUT"/core/*.o
avr-gcc -mmcu=atmega328p -Os -g -flto -fuse-linker-plugin -Wl,--gc-sections -o "$OUT/DeejMixer.elf" "$OUT/DeejMixer.o" "$OUT"/lib/*.o "$OUT/core.a" -lm
avr-objcopy -O ihex -R .eeprom "$OUT/DeejMixer.elf" "$OUT/DeejMixer.hex"
avr-size "$OUT/DeejMixer.elf"
