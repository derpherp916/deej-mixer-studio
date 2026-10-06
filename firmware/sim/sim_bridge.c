// Real-time simavr bridge: stdin bytes -> Nano UART RX, Nano UART TX -> stdout.
// Pots fixed at 100/50/25/0/100 %. Button D2 (Opera) is pressed at t=2.5 s for 120 ms and again at 4.0 s.
// On exit (stdin closed) prints LED refresh statistics to stderr.
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <fcntl.h>
#include <time.h>
#include <simavr/sim_avr.h>
#include <simavr/sim_elf.h>
#include <simavr/avr_uart.h>
#include <simavr/avr_adc.h>
#include <simavr/avr_ioport.h>

static avr_t *avr;
static long edges12, edges11;
static void uart_out(struct avr_irq_t *irq, uint32_t v, void *p) { unsigned char c = v; write(1, &c, 1); }
static void d12(struct avr_irq_t *irq, uint32_t v, void *p) { if (v) edges12++; }
static void d11(struct avr_irq_t *irq, uint32_t v, void *p) { if (v) edges11++; }
static double now_s(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return t.tv_sec + t.tv_nsec / 1e9; }

int main(int argc, char **argv) {
  elf_firmware_t fw = {0};
  if (elf_read_firmware(argv[1], &fw)) return 2;
  avr = avr_make_mcu_by_name("atmega328p");
  avr_init(avr); avr_load_firmware(avr, &fw);
  avr->frequency = 16000000; avr->avcc = 5000; avr->aref = 5000;
  uint32_t f = 0; avr_ioctl(avr, AVR_IOCTL_UART_GET_FLAGS('0'), &f);
  f &= ~AVR_UART_FLAG_STDIO; avr_ioctl(avr, AVR_IOCTL_UART_SET_FLAGS('0'), &f);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_UART_GETIRQ('0'), UART_IRQ_OUTPUT), uart_out, NULL);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('B'), 4), d12, NULL);
  avr_irq_register_notify(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('B'), 3), d11, NULL);
  int mv[5] = {5000, 2500, 1250, 0, 5000};
  for (int i = 0; i < 5; i++) avr_raise_irq(avr_io_getirq(avr, AVR_IOCTL_ADC_GETIRQ, ADC_IRQ_ADC0 + i), mv[i]);
  avr_irq_t *pd2 = avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('D'), 2);
  for (int i = 0; i < 6; i++) avr_raise_irq(avr_io_getirq(avr, AVR_IOCTL_IOPORT_GETIRQ('D'), 2 + i), 1);
  avr_irq_t *rx = avr_io_getirq(avr, AVR_IOCTL_UART_GETIRQ('0'), UART_IRQ_INPUT);
  fcntl(0, F_SETFL, O_NONBLOCK);
  double start = now_s();
  avr_cycle_count_t nextByte = 0;
  unsigned char inbuf[4096]; int inLen = 0, inPos = 0;
  for (;;) {
    double simT = avr->cycle / 16e6;
    double wall = now_s() - start;
    if (simT > wall) usleep((useconds_t)((simT - wall) * 1e6));
    if (inPos >= inLen) {
      int n = read(0, inbuf, sizeof inbuf);
      if (n == 0) break;               // stdin closed
      if (n > 0) { inLen = n; inPos = 0; }
    }
    if (inPos < inLen && avr->cycle >= nextByte) {   // 115200 baud = ~1389 cycles per byte
      avr_raise_irq(rx, inbuf[inPos++]); nextByte = avr->cycle + 1400;
    }
    int pressed = (simT > 2.5 && simT < 2.62) || (simT > 4.0 && simT < 4.12);
    avr_raise_irq(pd2, pressed ? 0 : 1);
    for (int k = 0; k < 2000; k++) avr_run(avr);
    if (simT > 30) break;
  }
  fprintf(stderr, "LED_EDGES D12=%ld D11=%ld\n", edges12, edges11);
  return 0;
}
