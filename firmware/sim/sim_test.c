// simavr harness: runs the real DeejMixer.elf on a simulated ATmega328P and checks the serial protocol.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <simavr/sim_avr.h>
#include <simavr/sim_elf.h>
#include <simavr/avr_uart.h>
#include <simavr/avr_adc.h>
#include <simavr/avr_ioport.h>

static avr_t *avr;
static char out[1 << 20];
static size_t outLen;
static int edgesD12, edgesD11;

static void uart_out(struct avr_irq_t *irq, uint32_t value, void *param) {
  if (outLen < sizeof(out) - 1) out[outLen++] = (char)value;
  if (getenv("DBGH") && value == 'H') fprintf(stderr, "H at %.1f ms\n", avr->cycle / 16000.0);
}
static void pin_d12(struct avr_irq_t *irq, uint32_t value, void *param) { if (value) edgesD12++; }
static void pin_d11(struct avr_irq_t *irq, uint32_t value, void *param) { if (value) edgesD11++; }

static void run_ms(double ms) {
  avr_cycle_count_t end = avr->cycle + (avr_cycle_count_t)(ms * 16000.0);
  while (avr->cycle < end) {
    int st = avr_run(avr);
    if (st == cpu_Done || st == cpu_Crashed) { printf("CPU stopped (%d)\n", st); exit(2); }
  }
}
static void set_pot(int ch, int mv) { avr_raise_irq(avr_io_getirq(avr, AVR_IOCTL_ADC_GETIRQ, ADC_IRQ_ADC0 + ch), mv); }
static void set_button(int idx, int pressed) {  // idx 0..5 => PD2..PD7
  avr_raise_irq(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('D'), 2 + idx), pressed ? 0 : 1);
}
static void send(const char *s) {
  avr_irq_t *in = avr_io_getirq(avr, AVR_IOCTL_UART_GETIRQ('0'), UART_IRQ_INPUT);
  for (; *s; s++) { avr_raise_irq(in, (uint8_t)*s); run_ms(0.1); }
}
static void send_cmd(const char *body, int corrupt) {
  unsigned sum = 0; for (const char *p = body; *p; p++) sum ^= (unsigned char)*p;
  char buf[96]; snprintf(buf, sizeof buf, "%s*%02X\n", body, corrupt ? (sum ^ 1) : sum);
  send(buf);
}
static int failures;
static void check(int cond, const char *what) { printf("%s %s\n", cond ? "PASS" : "FAIL", what); if (!cond) failures++; }
static size_t mark(void) { return outLen; }
static int seen_since(size_t from, const char *needle) { out[outLen] = 0; return strstr(out + from, needle) != NULL; }
static int count_since(size_t from, const char *needle) {
  out[outLen] = 0; int n = 0; const char *p = out + from;
  while ((p = strstr(p, needle))) { n++; p++; } return n;
}
static int last_frame(int v[6]) {
  out[outLen] = 0; char *p = out + outLen;
  while (p > out && !(p[0] == 'V' && p[1] == ',' && (p == out || p[-1] == '\n'))) p--;
  return sscanf(p, "V,%d,%d,%d,%d,%d,%d", &v[0], &v[1], &v[2], &v[3], &v[4], &v[5]) == 6;
}

int main(int argc, char **argv) {
  elf_firmware_t fw = {0};
  if (elf_read_firmware(argv[1], &fw)) { puts("cannot read elf"); return 2; }
  avr = avr_make_mcu_by_name("atmega328p");
  avr_init(avr);
  avr_load_firmware(avr, &fw);
  avr->frequency = 16000000; avr->avcc = 5000; avr->aref = 5000;
  // Disable simavr's stdio echo of UART output and the xon/xoff flow-control idle.
  uint32_t f = 0; avr_ioctl(avr, AVR_IOCTL_UART_GET_FLAGS('0'), &f);
  f &= ~AVR_UART_FLAG_STDIO; avr_ioctl(avr, AVR_IOCTL_UART_SET_FLAGS('0'), &f);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_UART_GETIRQ('0'), UART_IRQ_OUTPUT), uart_out, NULL);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('B'), 4), pin_d12, NULL);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('B'), 3), pin_d11, NULL);
  int mv[5] = {0, 1250, 2500, 3750, 5000};
  for (int i = 0; i < 5; i++) set_pot(i, mv[i]);
  for (int i = 0; i < 6; i++) set_button(i, 0);

  run_ms(300);
  check(seen_since(0, "HELLO,DEEJMIXER,3\r\n"), "boot banner");
  int v[6];
  check(last_frame(v), "value frames streaming");
  printf("     frame: %d %d %d %d %d mask=%d\n", v[0], v[1], v[2], v[3], v[4], v[5]);
  check(v[0] == 0 && abs(v[1] - 256) <= 4 && abs(v[2] - 512) <= 4 && abs(v[3] - 767) <= 4 && v[4] == 1023 && v[5] == 0,
        "pot values map 0/25/50/75/100% and buttons idle");
  size_t m = mark(); run_ms(200);
  int frames = count_since(m, "\nV,");
  check(frames >= 9 && frames <= 11, "~50 frames per second");

  // Pot moves: smoothing settles within ~150 ms.
  set_pot(2, 5000); run_ms(200); last_frame(v);
  check(v[2] == 1023, "pot A2 follows to full scale");
  set_pot(2, 2502); run_ms(40); int a = last_frame(v) ? v[2] : -1; run_ms(5); last_frame(v);
  check(a == v[2], "frame values stable (no jitter)");

  // Button press / release
  m = mark(); set_button(2, 1); run_ms(60); set_button(2, 0); run_ms(60);
  check(seen_since(m, "P,2\r\n") && seen_since(m, "R,2\r\n"), "button D4 press + release events");
  // 5 ms bounce must not produce events
  m = mark(); set_button(1, 1); run_ms(5); set_button(1, 0); run_ms(40);
  check(!seen_since(m, "P,1"), "5 ms glitch on D3 rejected by debounce");
  // 30 ms tap must be reported even though frames are 20 ms apart
  m = mark(); set_button(4, 1); run_ms(30); set_button(4, 0); run_ms(40);
  check(seen_since(m, "P,4") && seen_since(m, "R,4"), "short 30 ms tap on D6 reported");
  // All six buttons held -> mask 63
  for (int i = 0; i < 6; i++) set_button(i, 1);
  run_ms(60); last_frame(v); check(v[5] == 63, "all six buttons in mask");
  for (int i = 0; i < 6; i++) set_button(i, 0);
  run_ms(60);

  // Identify
  m = mark(); send("?\n"); run_ms(5);
  check(seen_since(m, "HELLO,DEEJMIXER,3"), "replies to ?");

  // LED commands: a valid L triggers a refresh on both data lines, a corrupt one does nothing.
  edgesD12 = edgesD11 = 0; send_cmd("L,7,2,2,2,2,40,200", 1); run_ms(5);
  check(edgesD12 == 0 && edgesD11 == 0, "corrupt checksum ignored");
  send_cmd("L,7,2,2,2,2,40,200", 0); run_ms(5);
  printf("     D12 edges %d, D11 edges %d\n", edgesD12, edgesD11);
  check(edgesD12 == 7 * 24 && edgesD11 == 4 * 24, "valid L drives 7 ring + 4 logo LEDs (24 bits each)");
  edgesD12 = edgesD11 = 0; send_cmd("L,7,2,2,2,2,40,200", 0); run_ms(5);
  check(edgesD12 == 0, "identical state does not re-send (keeps serial interrupts free)");
  send_cmd("C,4,FF0000", 0); run_ms(5);
  check(edgesD12 == 7 * 24, "colour change refreshes ring");
  edgesD12 = 0; send_cmd("L,9,2,2,2,2,40,200", 0); send_cmd("L,3,2,2,2,7,40,200", 0); send_cmd("X,1", 0); run_ms(5);
  check(edgesD12 == 0, "out-of-range / unknown commands ignored");
  // While host active, no HELLO spam
  m = mark(); for (int i = 0; i < 4; i++) { if (getenv("DBGH")) fprintf(stderr, "L at %.1f ms\n", avr->cycle/16000.0); send_cmd("L,7,2,2,2,2,40,200", 0); run_ms(500); }
  if (seen_since(m, "HELLO")) { out[outLen]=0; char *h=strstr(out+m,"HELLO"); printf("DEBUG ctx: %.200s\n", h-120>out+m?h-120:out+m); }
  check(!seen_since(m, "HELLO"), "no HELLO while PC is talking");
  // Steady state for 6 s with the PC refreshing every 500 ms: the LEDs must never be re-drawn (no flicker).
  edgesD12 = edgesD11 = 0;
  for (int i = 0; i < 12; i++) { send_cmd("L,7,2,2,2,2,40,200", 0); run_ms(500); }
  check(edgesD12 == 0 && edgesD11 == 0, "no LED flicker over 6 s of identical refreshes");
  // Host timeout: LEDs switch off, HELLO resumes
  edgesD12 = 0; m = mark(); run_ms(3600);
  check(edgesD12 == 7 * 24 && seen_since(m, "HELLO"), "after 3 s silence: LEDs refreshed (off) and HELLO resumes");
  // Test mode chase animates
  edgesD12 = edgesD11 = 0; send_cmd("T,4", 0); run_ms(2000);
  check(edgesD12 > 7 * 24 * 3 && edgesD11 > 4 * 24 * 3, "chase test pattern animates both chains");
  // Garbage robustness
  send("LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL\n");
  m = mark(); send("?\n"); run_ms(5);
  check(seen_since(m, "HELLO"), "survives overlong garbage line");

  printf("%s: %d failure(s)\n", failures ? "FAILED" : "ALL PASSED", failures);
  return failures ? 1 : 0;
}
