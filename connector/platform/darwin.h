#ifndef DOREMOTE_DARWIN_H
#define DOREMOTE_DARWIN_H

#include <stdint.h>

// Permission bits returned by dr_preflight.
#define DR_PERM_SCREEN 1
#define DR_PERM_INPUT 2

#define DR_MAX_DISPLAYS 32

// Isolation modes returned by dr_display_isolate.
#define DR_ISOLATION_DISABLED 1
#define DR_ISOLATION_MIRRORED 2

int dr_preflight(int prompt);

// Online display IDs.
int dr_list_displays(uint32_t *ids, int cap);
// Display configuration transactions; call only from a helper process.
int dr_display_isolate(uint32_t target, const uint32_t *ids, int n);
int dr_display_restore(int mode, const uint32_t *ids, int n);

void *dr_start(int width, int height, char *err, int errlen);
uint32_t dr_display_id(void *s);
int dr_start_capture(void *s, char *err, int errlen);
void dr_size(void *s, int *width, int *height);
// Returns 1 when a frame was copied, 0 on timeout, -1 when the stream stopped,
// -2 when dst is too small (w/h are still filled in).
int dr_next_frame(void *s, int timeout_ms, uint8_t *dst, int dst_cap, int *w, int *h);
void dr_stop(void *s);

void dr_mouse_move(void *s, int x, int y);
void dr_mouse_button(void *s, int button, int down);
void dr_scroll(void *s, int dx, int dy);
void dr_key(void *s, int keycode, int down);

#endif
