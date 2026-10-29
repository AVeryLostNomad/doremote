#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ApplicationServices/ApplicationServices.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include <dlfcn.h>
#include <pthread.h>
#include <sys/time.h>
#include "native_darwin.h"

// ---- Private CoreGraphics virtual display API (macOS 11+; used by DeskPad, BetterDisplay, Chromium tests). ----

@interface CGVirtualDisplayDescriptor : NSObject
@property(retain, nonatomic) dispatch_queue_t queue;
@property(retain, nonatomic) NSString *name;
@property(nonatomic) unsigned int maxPixelsHigh;
@property(nonatomic) unsigned int maxPixelsWide;
@property(nonatomic) CGSize sizeInMillimeters;
@property(nonatomic) unsigned int serialNum;
@property(nonatomic) unsigned int productID;
@property(nonatomic) unsigned int vendorID;
@property(copy, nonatomic) void (^terminationHandler)(id, id);
@end

@interface CGVirtualDisplayMode : NSObject
- (instancetype)initWithWidth:(unsigned int)width height:(unsigned int)height refreshRate:(double)refreshRate;
@end

@interface CGVirtualDisplaySettings : NSObject
@property(retain, nonatomic) NSArray *modes;
@property(nonatomic) unsigned int hiDPI;
@end

@interface CGVirtualDisplay : NSObject
@property(readonly, nonatomic) CGDirectDisplayID displayID;
- (instancetype)initWithDescriptor:(CGVirtualDisplayDescriptor *)descriptor;
- (BOOL)applySettings:(CGVirtualDisplaySettings *)settings;
@end

// Private SkyLight call that turns a display off for a configuration transaction.
typedef CGError (*CGSConfigureDisplayEnabledFn)(CGDisplayConfigRef, CGDirectDisplayID, bool);

static CGSConfigureDisplayEnabledFn displayEnabledFn(void) {
    static CGSConfigureDisplayEnabledFn fn;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        dlopen("/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight", RTLD_LAZY);
        fn = (CGSConfigureDisplayEnabledFn)dlsym(RTLD_DEFAULT, "CGSConfigureDisplayEnabled");
    });
    return fn;
}

static void seterr(char *err, int errlen, NSString *msg) {
    if (err && errlen > 0) snprintf(err, (size_t)errlen, "%s", msg.UTF8String);
}

// ---- Capture ----

@interface DRCapture : NSObject <SCStreamOutput, SCStreamDelegate>
@property(strong) SCStream *stream;
@end

@implementation DRCapture {
@public
    pthread_mutex_t mu;
    pthread_cond_t cond;
    CVPixelBufferRef latest;
    uint64_t seq;
    int stopped;
}

- (instancetype)init {
    if ((self = [super init])) {
        pthread_mutex_init(&mu, NULL);
        pthread_cond_init(&cond, NULL);
    }
    return self;
}

- (void)dealloc {
    if (latest) CVPixelBufferRelease(latest);
    pthread_mutex_destroy(&mu);
    pthread_cond_destroy(&cond);
}

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
    if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sb)) return;
    CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sb, false);
    if (!attachments || CFArrayGetCount(attachments) == 0) return;
    NSDictionary *info = (__bridge NSDictionary *)CFArrayGetValueAtIndex(attachments, 0);
    NSNumber *status = info[SCStreamFrameInfoStatus];
    if (!status || status.integerValue != SCFrameStatusComplete) return;
    CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sb);
    if (!pb) return;
    CVPixelBufferRetain(pb);
    pthread_mutex_lock(&mu);
    if (latest) CVPixelBufferRelease(latest);
    latest = pb;
    seq++;
    pthread_cond_broadcast(&cond);
    pthread_mutex_unlock(&mu);
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    NSLog(@"doremote: capture stopped: %@", error);
    pthread_mutex_lock(&mu);
    stopped = 1;
    pthread_cond_broadcast(&cond);
    pthread_mutex_unlock(&mu);
}
@end

// ---- Session ----

typedef struct {
    CGVirtualDisplay *display;
    CGDirectDisplayID displayID;
    int width, height;
    DRCapture *capture;
    dispatch_queue_t queue;
    uint64_t lastSeq;

    CGEventSourceRef source;
    int buttons;           // bitmask of held DOM buttons
    CGEventFlags flags;    // held modifier flags
    int capsLock;
    double lastClickTime;
    int lastClickButton;
    CGPoint lastClickPos;
    int clickCount;
    CGPoint pos;
} DRSession;

int dr_preflight(int prompt) {
    int perms = 0;
    if (CGPreflightScreenCaptureAccess()) perms |= DR_PERM_SCREEN;
    else if (prompt) CGRequestScreenCaptureAccess();
    NSDictionary *opts = @{(__bridge NSString *)kAXTrustedCheckOptionPrompt: @(prompt ? YES : NO)};
    if (AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)opts)) perms |= DR_PERM_INPUT;
    return perms;
}

static int displayOnline(CGDirectDisplayID id) {
    CGDirectDisplayID list[32];
    uint32_t n = 0;
    if (CGGetOnlineDisplayList(32, list, &n) != kCGErrorSuccess) return 0;
    for (uint32_t i = 0; i < n; i++) if (list[i] == id) return 1;
    return 0;
}

static int currentSizeMatches(DRSession *s) {
    return (int)CGDisplayPixelsWide(s->displayID) == s->width && (int)CGDisplayPixelsHigh(s->displayID) == s->height;
}

static CGError setEnabled(const CGDirectDisplayID *ids, int n, bool enabled, CGConfigureOption option) {
    CGSConfigureDisplayEnabledFn fn = displayEnabledFn();
    if (!fn) return kCGErrorNotImplemented;
    CGDisplayConfigRef cfg;
    CGError e = CGBeginDisplayConfiguration(&cfg);
    if (e != kCGErrorSuccess) return e;
    for (int i = 0; i < n && e == kCGErrorSuccess; i++) e = fn(cfg, ids[i], enabled);
    if (e != kCGErrorSuccess) {
        CGCancelDisplayConfiguration(cfg);
        return e;
    }
    return CGCompleteDisplayConfiguration(cfg, option);
}

int dr_list_displays(uint32_t *ids, int cap) {
    uint32_t n = 0;
    if (CGGetOnlineDisplayList((uint32_t)cap, ids, &n) != kCGErrorSuccess) return 0;
    return (int)n;
}

// Display isolation entry points. They run in a short-lived helper process (see
// runDisplayCtl in platform_darwin.go): after a display configuration transaction, the calling process's
// CoreGraphics view of the display list stops updating, so the long-lived connector would
// never see its next virtual display come online.

// Turns the given displays off; if the private SkyLight call is unavailable or refused,
// mirrors them onto `target` instead. Returns DR_ISOLATION_* on success or -CGError.
int dr_display_isolate(uint32_t target, const uint32_t *ids, int n) {
    CGError e = setEnabled(ids, n, false, kCGConfigureForSession);
    if (e == kCGErrorSuccess) return DR_ISOLATION_DISABLED;
    NSLog(@"doremote: disabling displays failed (%d); mirroring them instead", e);

    CGDisplayConfigRef cfg;
    e = CGBeginDisplayConfiguration(&cfg);
    if (e != kCGErrorSuccess) return -e;
    for (int i = 0; i < n && e == kCGErrorSuccess; i++) e = CGConfigureDisplayMirrorOfDisplay(cfg, ids[i], target);
    if (e == kCGErrorSuccess) e = CGCompleteDisplayConfiguration(cfg, kCGConfigureForSession);
    else CGCancelDisplayConfiguration(cfg);
    return e == kCGErrorSuccess ? DR_ISOLATION_MIRRORED : -e;
}

// Undoes dr_display_isolate. Returns a CGError.
int dr_display_restore(int mode, const uint32_t *ids, int n) {
    if (mode == DR_ISOLATION_DISABLED) return setEnabled(ids, n, true, kCGConfigureForSession);
    CGDisplayConfigRef cfg;
    CGError e = CGBeginDisplayConfiguration(&cfg);
    if (e != kCGErrorSuccess) return e;
    for (int i = 0; i < n; i++) CGConfigureDisplayMirrorOfDisplay(cfg, ids[i], kCGNullDirectDisplay);
    return CGCompleteDisplayConfiguration(cfg, kCGConfigureForSession);
}

static void stopSession(DRSession *s) {
    if (s->capture.stream) {
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        [s->capture.stream stopCaptureWithCompletionHandler:^(NSError *error) { dispatch_semaphore_signal(done); }];
        dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
        s->capture.stream = nil;
    }
    s->display = nil; // releasing the CGVirtualDisplay removes it
    s->capture = nil;
    if (s->source) {
        CFRelease(s->source);
        s->source = NULL;
    }
}

static NSString *startCapture(DRSession *s) {
    __block SCDisplay *target = nil;
    __block NSError *listErr = nil;
    // The virtual display can take a moment to show up in shareable content.
    for (int attempt = 0; attempt < 20 && !target; attempt++) {
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        [SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *content, NSError *error) {
            listErr = error;
            for (SCDisplay *d in content.displays) {
                if (d.displayID == s->displayID) target = d;
            }
            dispatch_semaphore_signal(done);
        }];
        dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
        if (listErr) break;
        if (!target) usleep(150 * 1000);
    }
    if (listErr) return [NSString stringWithFormat:@"screen capture unavailable (grant Screen Recording permission): %@", listErr.localizedDescription];
    if (!target) return @"virtual display not visible to ScreenCaptureKit";

    SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:target excludingWindows:@[]];
    SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
    cfg.width = (size_t)s->width;
    cfg.height = (size_t)s->height;
    cfg.pixelFormat = kCVPixelFormatType_32BGRA;
    cfg.minimumFrameInterval = CMTimeMake(1, 60);
    cfg.queueDepth = 4;
    cfg.showsCursor = YES;
    cfg.colorSpaceName = kCGColorSpaceSRGB;

    s->capture = [[DRCapture alloc] init];
    SCStream *stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:s->capture];
    NSError *addErr = nil;
    if (![stream addStreamOutput:s->capture type:SCStreamOutputTypeScreen sampleHandlerQueue:s->queue error:&addErr]) {
        return [NSString stringWithFormat:@"capture setup failed: %@", addErr.localizedDescription];
    }
    __block NSError *startErr = nil;
    dispatch_semaphore_t started = dispatch_semaphore_create(0);
    [stream startCaptureWithCompletionHandler:^(NSError *error) {
        startErr = error;
        dispatch_semaphore_signal(started);
    }];
    dispatch_semaphore_wait(started, DISPATCH_TIME_FOREVER);
    if (startErr) return [NSString stringWithFormat:@"capture start failed: %@", startErr.localizedDescription];
    s->capture.stream = stream;
    return nil;
}

// Creates the virtual display and waits for it to come online. Capture starts separately
// (dr_start_capture) so other displays can be isolated first.
void *dr_start(int width, int height, char *err, int errlen) {
    @autoreleasepool {
        Class descCls = NSClassFromString(@"CGVirtualDisplayDescriptor");
        Class modeCls = NSClassFromString(@"CGVirtualDisplayMode");
        Class settingsCls = NSClassFromString(@"CGVirtualDisplaySettings");
        Class displayCls = NSClassFromString(@"CGVirtualDisplay");
        if (!descCls || !modeCls || !settingsCls || !displayCls) {
            seterr(err, errlen, @"this macOS version has no CGVirtualDisplay support");
            return NULL;
        }

        DRSession *s = calloc(1, sizeof(DRSession));
        s->width = width;
        s->height = height;
        s->queue = dispatch_queue_create("dev.doremote.display", DISPATCH_QUEUE_SERIAL);

        CGVirtualDisplayDescriptor *desc = [[descCls alloc] init];
        desc.queue = s->queue;
        desc.name = @"doremote";
        desc.maxPixelsWide = (unsigned int)width;
        desc.maxPixelsHigh = (unsigned int)height;
        // ~110 DPI so macOS picks a sensible default scale.
        desc.sizeInMillimeters = CGSizeMake(width * 25.4 / 110.0, height * 25.4 / 110.0);
        desc.vendorID = 0x3456;
        desc.productID = 0x1234;
        desc.serialNum = (unsigned int)arc4random();
        desc.terminationHandler = ^(id a, id b) { NSLog(@"doremote: virtual display terminated"); };

        s->display = [[displayCls alloc] initWithDescriptor:desc];
        if (!s->display) {
            seterr(err, errlen, @"failed to create virtual display");
            stopSession(s);
            free(s);
            return NULL;
        }
        CGVirtualDisplaySettings *settings = [[settingsCls alloc] init];
        settings.hiDPI = 0;
        settings.modes = @[[[modeCls alloc] initWithWidth:(unsigned int)width height:(unsigned int)height refreshRate:60]];
        if (![s->display applySettings:settings]) {
            seterr(err, errlen, @"virtual display rejected the requested resolution");
            stopSession(s);
            free(s);
            return NULL;
        }
        s->displayID = s->display.displayID;

        for (int i = 0; i < 60 && !(displayOnline(s->displayID) && currentSizeMatches(s)); i++) usleep(50 * 1000);
        if (!displayOnline(s->displayID)) {
            seterr(err, errlen, @"virtual display did not come online");
            stopSession(s);
            free(s);
            return NULL;
        }
        s->source = CGEventSourceCreate(kCGEventSourceStatePrivate);
        return s;
    }
}

uint32_t dr_display_id(void *p) {
    return ((DRSession *)p)->displayID;
}

int dr_start_capture(void *p, char *err, int errlen) {
    DRSession *s = p;
    @autoreleasepool {
        NSString *e = startCapture(s);
        if (e) {
            seterr(err, errlen, e);
            return 0;
        }
        CGRect b = CGDisplayBounds(s->displayID);
        s->pos = CGPointMake(CGRectGetMidX(b), CGRectGetMidY(b));
        return 1;
    }
}

void dr_size(void *p, int *w, int *h) {
    DRSession *s = p;
    *w = s->width;
    *h = s->height;
}

int dr_next_frame(void *p, int timeout_ms, uint8_t *dst, int dst_cap, int *w, int *h) {
    DRSession *s = p;
    DRCapture *c = s->capture;
    if (!c) return -1;

    struct timeval now;
    gettimeofday(&now, NULL);
    struct timespec deadline;
    long long ns = (long long)now.tv_usec * 1000 + (long long)timeout_ms * 1000000LL;
    deadline.tv_sec = now.tv_sec + (time_t)(ns / 1000000000LL);
    deadline.tv_nsec = (long)(ns % 1000000000LL);

    pthread_mutex_lock(&c->mu);
    while (c->seq == s->lastSeq && !c->stopped) {
        if (pthread_cond_timedwait(&c->cond, &c->mu, &deadline) != 0) break;
    }
    if (c->stopped) {
        pthread_mutex_unlock(&c->mu);
        return -1;
    }
    if (c->seq == s->lastSeq || !c->latest) {
        pthread_mutex_unlock(&c->mu);
        return 0;
    }
    CVPixelBufferRef pb = CVPixelBufferRetain(c->latest);
    uint64_t seq = c->seq;
    pthread_mutex_unlock(&c->mu);

    int pw = (int)CVPixelBufferGetWidth(pb);
    int ph = (int)CVPixelBufferGetHeight(pb);
    *w = pw;
    *h = ph;
    if (pw * ph * 4 > dst_cap) {
        CVPixelBufferRelease(pb);
        return -2;
    }
    CVPixelBufferLockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
    const uint8_t *src = CVPixelBufferGetBaseAddress(pb);
    size_t stride = CVPixelBufferGetBytesPerRow(pb);
    size_t row = (size_t)pw * 4;
    if (stride == row) {
        memcpy(dst, src, row * (size_t)ph);
    } else {
        for (int y = 0; y < ph; y++) memcpy(dst + (size_t)y * row, src + (size_t)y * stride, row);
    }
    CVPixelBufferUnlockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
    CVPixelBufferRelease(pb);
    s->lastSeq = seq;
    return 1;
}

void dr_stop(void *p) {
    DRSession *s = p;
    @autoreleasepool {
        stopSession(s);
    }
    free(s);
}

// ---- Input ----

static CGPoint toGlobal(DRSession *s, int x, int y) {
    CGRect b = CGDisplayBounds(s->displayID);
    double gx = b.origin.x + (double)x * b.size.width / s->width;
    double gy = b.origin.y + (double)y * b.size.height / s->height;
    return CGPointMake(gx, gy);
}

static void post(DRSession *s, CGEventRef ev) {
    if (!ev) return;
    CGEventSetFlags(ev, s->flags | (CGEventGetFlags(ev) & kCGEventFlagMaskNonCoalesced));
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

void dr_mouse_move(void *p, int x, int y) {
    DRSession *s = p;
    s->pos = toGlobal(s, x, y);
    CGEventType type = kCGEventMouseMoved;
    CGMouseButton btn = kCGMouseButtonLeft;
    if (s->buttons & 1) type = kCGEventLeftMouseDragged;
    else if (s->buttons & 4) { type = kCGEventRightMouseDragged; btn = kCGMouseButtonRight; }
    else if (s->buttons & 2) { type = kCGEventOtherMouseDragged; btn = kCGMouseButtonCenter; }
    post(s, CGEventCreateMouseEvent(s->source, type, s->pos, btn));
}

void dr_mouse_button(void *p, int button, int down) {
    DRSession *s = p;
    CGEventType type;
    CGMouseButton btn;
    switch (button) {
    case 0: type = down ? kCGEventLeftMouseDown : kCGEventLeftMouseUp; btn = kCGMouseButtonLeft; break;
    case 2: type = down ? kCGEventRightMouseDown : kCGEventRightMouseUp; btn = kCGMouseButtonRight; break;
    case 1: type = down ? kCGEventOtherMouseDown : kCGEventOtherMouseUp; btn = kCGMouseButtonCenter; break;
    default: return;
    }
    int bit = 1 << button;
    if (down) {
        double t = CFAbsoluteTimeGetCurrent();
        double dx = s->pos.x - s->lastClickPos.x, dy = s->pos.y - s->lastClickPos.y;
        if (button == s->lastClickButton && t - s->lastClickTime < 0.5 && dx * dx + dy * dy < 25) s->clickCount++;
        else s->clickCount = 1;
        s->lastClickTime = t;
        s->lastClickButton = button;
        s->lastClickPos = s->pos;
        s->buttons |= bit;
    } else {
        if (!(s->buttons & bit)) return;
        s->buttons &= ~bit;
    }
    CGEventRef ev = CGEventCreateMouseEvent(s->source, type, s->pos, btn);
    if (ev) CGEventSetIntegerValueField(ev, kCGMouseEventClickState, s->clickCount);
    post(s, ev);
}

void dr_scroll(void *p, int dx, int dy) {
    DRSession *s = p;
    CGEventRef ev = CGEventCreateScrollWheelEvent2(s->source, kCGScrollEventUnitPixel, 2, -dy, -dx, 0);
    if (ev) CGEventSetLocation(ev, s->pos);
    post(s, ev);
}

static CGEventFlags modifierMask(int keycode) {
    // Generic mask | device-dependent left/right bit (IOKit NX_DEVICE*KEYMASK).
    switch (keycode) {
    case 56: return kCGEventFlagMaskShift | 0x02;
    case 60: return kCGEventFlagMaskShift | 0x04;
    case 59: return kCGEventFlagMaskControl | 0x01;
    case 62: return kCGEventFlagMaskControl | 0x2000;
    case 58: return kCGEventFlagMaskAlternate | 0x20;
    case 61: return kCGEventFlagMaskAlternate | 0x40;
    case 55: return kCGEventFlagMaskCommand | 0x08;
    case 54: return kCGEventFlagMaskCommand | 0x10;
    default: return 0;
    }
}

static CGEventFlags recomputeFlags(CGEventFlags held, int capsLock) {
    CGEventFlags f = held & (0x02 | 0x04 | 0x01 | 0x2000 | 0x20 | 0x40 | 0x08 | 0x10);
    if (f & (0x02 | 0x04)) f |= kCGEventFlagMaskShift;
    if (f & (0x01 | 0x2000)) f |= kCGEventFlagMaskControl;
    if (f & (0x20 | 0x40)) f |= kCGEventFlagMaskAlternate;
    if (f & (0x08 | 0x10)) f |= kCGEventFlagMaskCommand;
    if (capsLock) f |= kCGEventFlagMaskAlphaShift;
    return f;
}

void dr_key(void *p, int keycode, int down) {
    DRSession *s = p;
    CGEventFlags mask = modifierMask(keycode);
    if (mask || keycode == 57) {
        if (keycode == 57) {
            if (!down) return; // caps lock toggles on press
            s->capsLock = !s->capsLock;
        } else if (down) {
            s->flags |= mask;
        } else {
            s->flags &= ~(mask & ~(CGEventFlags)(kCGEventFlagMaskShift | kCGEventFlagMaskControl | kCGEventFlagMaskAlternate | kCGEventFlagMaskCommand));
        }
        s->flags = recomputeFlags(s->flags, s->capsLock);
        CGEventRef ev = CGEventCreateKeyboardEvent(s->source, (CGKeyCode)keycode, down ? true : false);
        if (ev) CGEventSetType(ev, kCGEventFlagsChanged);
        post(s, ev);
        return;
    }
    post(s, CGEventCreateKeyboardEvent(s->source, (CGKeyCode)keycode, down ? true : false));
}
