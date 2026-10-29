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

// dr_next_video results.
#define DR_VIDEO_FRAME 1
#define DR_VIDEO_TIMEOUT 0
#define DR_VIDEO_STOPPED -1
#define DR_VIDEO_ERROR -2

typedef struct {
    const uint8_t *data; // Annex B access unit, owned by the session, valid until the next dr_next_video/dr_stop
    int len;
    int key;             // IDR with SPS/PPS in-band
    int refined;         // re-encoding of the previous picture, not a new one
    int width, height;
    int status;          // OSStatus for DR_VIDEO_ERROR
} dr_video;

// Waits up to timeout_ms for a new screen picture (or a keyframe request) and encodes it
// synchronously. With refine set, a timeout re-encodes the last picture instead. Call from one
// thread at a time; nothing is encoded between calls.
int dr_next_video(void *s, int timeout_ms, int refine, dr_video *out);
// Thread-safe; both take effect on the next encoded frame (a pending dr_next_video wakes for a keyframe).
void dr_request_keyframe(void *s);
void dr_set_bitrate(void *s, int bps);
void dr_stop(void *s);

void dr_mouse_move(void *s, int x, int y);
void dr_mouse_button(void *s, int button, int down);
void dr_scroll(void *s, int dx, int dy);
void dr_key(void *s, int keycode, int down);

#endif
