/*
  Deej Mixer firmware v3 — Arduino Nano (ATmega328P, 16 MHz)

  Wiring (facing the controls, big knob on the LEFT):
    A0 = big knob            A1..A4 = small knobs, nearest -> farthest from the big knob
    D2 = Opera logo button   D3 = Steam logo   D4 = Discord logo   D5 = Spotify logo
    D6 = small LEFT button   D7 = small RIGHT button      (all buttons: pin -> button -> GND)
    D12 = DIN of the 7-LED ring around the big knob (WS2812B)
    D11 = DIN of the 4 logo LEDs, chained DIN->DOUT nearest -> farthest (WS2812B)
    Pots: outer legs to 5V and GND (NOT VIN), middle leg (wiper) to its A pin.

  Serial protocol, 115200 baud, one line per message, '\n' terminated.
    Nano -> PC
      HELLO,DEEJMIXER,3                 on boot, every second while no PC is talking, and in reply to "?"
      V,a0,a1,a2,a3,a4,mask             every 20 ms. a = 0..1023 (smoothed), mask bit i = button i held
      P,i  /  R,i                       button i (0..5 = D2..D7) pressed / released (debounced, never missed)
    PC -> Nano   (every line except "?" ends with *HH = XOR of all bytes before '*', in hex)
      ?                                 identify
      C,i,RRGGBB                        set colour: i = 0..3 logo LEDs, 4 = ring
      L,level,s0,s1,s2,s3,rb,lb         ring: first `level` (0..7) LEDs lit; logo state s = 0 off, 1 dim, 2 on;
                                        rb / lb = ring / logo brightness 0..255 (ring capped at RING_MAX)
      T,mode                            LED test: 0 = normal, 1 = all white, 2 = ring only, 3 = logos only,
                                        4 = chase each LED in order, 5 = colour cycle
    If the PC stops talking for 3 s the LEDs switch off.

  No-PC wiring check: hold the Opera button (D2) while plugging in USB.
    Ring = position of whichever knob you last moved, logos light while D2..D5 are held,
    D6 lights logo 1+2, D7 lights logo 3+4. Unplug to leave this mode.
*/
#include <Adafruit_NeoPixel.h>

const uint8_t POT_PINS[5] = {A0, A1, A2, A3, A4};
const uint8_t BUTTON_PINS[6] = {2, 3, 4, 5, 6, 7};
const uint8_t RING_PIN = 12, RING_COUNT = 7;
const uint8_t LOGO_PIN = 11, LOGO_COUNT = 4;
const uint8_t RING_MAX = 160;           // 7 ring LEDs; logos (4 LEDs) may use full brightness.
                                        // Worst case (all white) stays near 450 mA, inside USB limits.
const uint16_t HOST_TIMEOUT_MS = 3000;
const uint8_t DEBOUNCE_MS = 15;
const uint8_t FRAME_MS = 20;

Adafruit_NeoPixel ring(RING_COUNT, RING_PIN, NEO_GRB + NEO_KHZ800);
Adafruit_NeoPixel logos(LOGO_COUNT, LOGO_PIN, NEO_GRB + NEO_KHZ800);

// ---------- inputs ----------
uint16_t smooth[5];          // value * 4 (fixed point) for an IIR filter
uint16_t reported[5];        // value last reported (hysteresis)
bool btnRaw[6], btnStable[6];
unsigned long btnChanged[6];

// ---------- LED state ----------
uint32_t colors[5] = {0xFF1B2D, 0x66C0F4, 0x5865F2, 0x1DB954, 0x30A0FF};
uint8_t ringLevel = 0, logoState[4] = {0, 0, 0, 0}, ringBright = 100, logoBright = 200, testMode = 0;
bool ledsDirty = true, hostActive = false, offlineCheck = false;
unsigned long lastHost = 0, lastHello = 0, lastFrame = 0, lastAnim = 0;
uint8_t animStep = 0, lastMovedPot = 0;

char line[48];
uint8_t lineLen = 0;
bool lineOverflow = false;

static uint32_t scale(uint32_t c, uint8_t num, uint8_t den) {
  uint8_t r = (uint8_t)(((c >> 16) & 0xFF) * num / den);
  uint8_t g = (uint8_t)(((c >> 8) & 0xFF) * num / den);
  uint8_t b = (uint8_t)((c & 0xFF) * num / den);
  return ((uint32_t)r << 16) | ((uint32_t)g << 8) | b;
}

static uint16_t readPot(uint8_t i) {
  analogRead(POT_PINS[i]);                // discard first sample after mux switch
  uint16_t sum = 0;
  for (uint8_t k = 0; k < 4; k++) sum += analogRead(POT_PINS[i]);
  return sum / 4;
}

void renderLeds() {
  ring.clear();
  logos.clear();
  uint8_t rb = ringBright > RING_MAX ? RING_MAX : ringBright, lb = logoBright;
  if (testMode || offlineCheck) { rb = 120; lb = 200; }   // test patterns must be clearly visible
  ring.setBrightness(rb);
  logos.setBrightness(lb);

  if (offlineCheck) {
    uint8_t lit = (uint8_t)(((uint32_t)reported[lastMovedPot] * RING_COUNT + 1022) / 1023);
    for (uint8_t i = 0; i < lit; i++) ring.setPixelColor(i, colors[4]);
    for (uint8_t i = 0; i < 4; i++) if (btnStable[i]) logos.setPixelColor(i, colors[i]);
    if (btnStable[4]) { logos.setPixelColor(0, 0xFFFFFF); logos.setPixelColor(1, 0xFFFFFF); }
    if (btnStable[5]) { logos.setPixelColor(2, 0xFFFFFF); logos.setPixelColor(3, 0xFFFFFF); }
  } else if (testMode == 1) {
    for (uint8_t i = 0; i < RING_COUNT; i++) ring.setPixelColor(i, 0xFFFFFF);
    for (uint8_t i = 0; i < LOGO_COUNT; i++) logos.setPixelColor(i, 0xFFFFFF);
  } else if (testMode == 2) {
    for (uint8_t i = 0; i < RING_COUNT; i++) ring.setPixelColor(i, 0xFFFFFF);
  } else if (testMode == 3) {
    for (uint8_t i = 0; i < LOGO_COUNT; i++) logos.setPixelColor(i, colors[i]);
  } else if (testMode == 4) {
    uint8_t n = animStep % (RING_COUNT + LOGO_COUNT);
    if (n < RING_COUNT) ring.setPixelColor(n, 0xFFFFFF);
    else logos.setPixelColor(n - RING_COUNT, colors[n - RING_COUNT]);
  } else if (testMode == 5) {
    const uint32_t cycle[4] = {0xFF0000, 0x00FF00, 0x0000FF, 0xFFFFFF};
    uint32_t c = cycle[animStep % 4];
    for (uint8_t i = 0; i < RING_COUNT; i++) ring.setPixelColor(i, c);
    for (uint8_t i = 0; i < LOGO_COUNT; i++) logos.setPixelColor(i, c);
  } else if (hostActive) {
    for (uint8_t i = 0; i < ringLevel && i < RING_COUNT; i++) ring.setPixelColor(i, colors[4]);
    for (uint8_t i = 0; i < LOGO_COUNT; i++) {
      if (logoState[i] == 2) logos.setPixelColor(i, colors[i]);
      else if (logoState[i] == 1) logos.setPixelColor(i, scale(colors[i], 1, 5));
    }
  }
  ring.show();
  logos.show();
  ledsDirty = false;
}

static bool parseUInt(char *&p, uint16_t &out, uint16_t maxValue) {
  if (*p < '0' || *p > '9') return false;
  uint32_t v = 0;
  while (*p >= '0' && *p <= '9') { v = v * 10 + (*p - '0'); if (v > maxValue) return false; p++; }
  out = (uint16_t)v;
  return true;
}

static bool expectComma(char *&p) { if (*p != ',') return false; p++; return true; }

static int8_t hexDigit(char h) {
  if (h >= '0' && h <= '9') return h - '0';
  if (h >= 'A' && h <= 'F') return h - 'A' + 10;
  if (h >= 'a' && h <= 'f') return h - 'a' + 10;
  return -1;
}

void handleLine() {
  char *p = line;
  if (line[0] == '?' && line[1] == 0) { Serial.println(F("HELLO,DEEJMIXER,3")); return; }
  // Verify and strip the checksum. LED updates briefly block interrupts, so a received byte can be
  // lost; a damaged command is ignored and the PC's periodic refresh repairs the state.
  char *star = strchr(line, '*');
  if (!star || star[1] == 0 || star[2] == 0 || star[3] != 0) return;
  int8_t hi = hexDigit(star[1]), lo = hexDigit(star[2]);
  if (hi < 0 || lo < 0) return;
  uint8_t sum = 0;
  for (char *q = line; q < star; q++) sum ^= (uint8_t)*q;
  if (sum != (uint8_t)((hi << 4) | lo)) return;
  *star = 0;
  if (line[1] != ',') return;
  char cmd = line[0];
  p = line + 2;
  uint16_t a, v;
  (void)a;
  if (cmd == 'C') {
    if (!parseUInt(p, a, 4) || !expectComma(p)) return;
    uint32_t c = 0;
    for (uint8_t k = 0; k < 6; k++) {
      int8_t d = hexDigit(*p++);
      if (d < 0) return;
      c = (c << 4) | (uint8_t)d;
    }
    if (*p) return;
    if (colors[a] != c) { colors[a] = c; ledsDirty = true; }
  } else if (cmd == 'L') {
    uint16_t lvl, s[4], rb, lb;
    if (!parseUInt(p, lvl, RING_COUNT)) return;
    for (uint8_t i = 0; i < 4; i++) if (!expectComma(p) || !parseUInt(p, s[i], 2)) return;
    if (!expectComma(p) || !parseUInt(p, rb, 255) || !expectComma(p) || !parseUInt(p, lb, 255) || *p) return;
    if (ringLevel != lvl || ringBright != rb || logoBright != lb) ledsDirty = true;
    ringLevel = lvl; ringBright = rb; logoBright = lb;
    for (uint8_t i = 0; i < 4; i++) { if (logoState[i] != s[i]) ledsDirty = true; logoState[i] = s[i]; }
  } else if (cmd == 'T') {
    if (!parseUInt(p, v, 5) || *p) return;
    if (testMode != v) { testMode = v; animStep = 0; ledsDirty = true; }
  } else return;
  lastHost = millis();
  if (!hostActive) { hostActive = true; ledsDirty = true; }
}

void setup() {
  Serial.begin(115200);
  for (uint8_t i = 0; i < 6; i++) {
    pinMode(BUTTON_PINS[i], INPUT_PULLUP);
  }
  delay(5);
  for (uint8_t i = 0; i < 6; i++) {
    btnRaw[i] = btnStable[i] = digitalRead(BUTTON_PINS[i]) == LOW;
    btnChanged[i] = 0;
  }
  for (uint8_t i = 0; i < 5; i++) { uint16_t r = readPot(i); smooth[i] = r * 4; reported[i] = r; }
  offlineCheck = btnStable[0];             // Opera button held while powering up
  ring.begin();
  logos.begin();
  renderLeds();
  Serial.println(F("HELLO,DEEJMIXER,3"));
}

void loop() {
  unsigned long now = millis();

  while (Serial.available()) {
    char c = Serial.read();
    if (c == '\n') {
      line[lineLen] = 0;
      if (!lineOverflow && lineLen) handleLine();
      lineLen = 0; lineOverflow = false;
    } else if (c != '\r') {
      if (lineLen < sizeof(line) - 1) line[lineLen++] = c; else lineOverflow = true;
    }
  }

  // Buttons: debounce, emit edge events immediately so short taps are never lost.
  for (uint8_t i = 0; i < 6; i++) {
    bool r = digitalRead(BUTTON_PINS[i]) == LOW;
    if (r != btnRaw[i]) { btnRaw[i] = r; btnChanged[i] = now; }
    if (btnStable[i] != btnRaw[i] && now - btnChanged[i] >= DEBOUNCE_MS) {
      btnStable[i] = btnRaw[i];
      Serial.print(btnStable[i] ? F("P,") : F("R,"));
      Serial.println(i);
      if (offlineCheck) ledsDirty = true;
    }
  }

  if (now - lastFrame >= FRAME_MS) {
    lastFrame = now;
    uint8_t mask = 0;
    for (uint8_t i = 0; i < 6; i++) if (btnStable[i]) mask |= 1 << i;
    Serial.print(F("V"));
    for (uint8_t i = 0; i < 5; i++) {
      // Smoothing: fast moves are followed instantly, small changes are filtered (IIR, alpha 1/4).
      uint16_t raw = readPot(i);
      int16_t jump = (int16_t)raw - (int16_t)(smooth[i] / 4);
      if (jump > 24 || jump < -24) smooth[i] = raw * 4;
      else smooth[i] = smooth[i] - smooth[i] / 4 + raw;
      uint16_t v = (smooth[i] + 2) / 4;
      if (v <= 4) v = 0;
      if (v >= 1019) v = 1023;
      int16_t diff = (int16_t)v - (int16_t)reported[i];
      if (diff >= 3 || diff <= -3 || ((v == 0 || v == 1023) && v != reported[i])) {
        reported[i] = v;
        if (offlineCheck) { lastMovedPot = i; ledsDirty = true; }
      }
      Serial.print(',');
      Serial.print(reported[i]);
    }
    Serial.print(',');
    Serial.println(mask);
  }

  // millis() again: lastHost may have been set after `now` was taken, and an unsigned "now - lastHost"
  // would then wrap around and switch the LEDs off by mistake.
  if (hostActive && millis() - lastHost > HOST_TIMEOUT_MS) {
    hostActive = false; testMode = 0; ledsDirty = true;
  }
  if (!hostActive && now - lastHello >= 1000) {
    lastHello = now;
    Serial.println(F("HELLO,DEEJMIXER,3"));
  }
  if ((testMode == 4 || testMode == 5) && now - lastAnim >= (testMode == 4 ? 350 : 600)) {
    lastAnim = now; animStep++; ledsDirty = true;
  }
  if (ledsDirty) renderLeds();
}
