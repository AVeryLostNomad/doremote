package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/joey/doremote/connector/platform"
)

const (
	// maxInflight bounds frames sent but not yet acknowledged by the viewer. Encoding waits for
	// a free slot instead of queueing frames, so a slow link costs frame rate, not latency;
	// the rate controller lowers the bitrate so the slots free up quickly again.
	maxInflight = 4
	// sendBuffers holds the frame being written, one queued behind it and one being encoded.
	sendBuffers = 3

	minFrameInterval = time.Second / platform.MaxFrameRate

	// Video message header: u8 kind, u8 flags, u16 width, u16 height (little-endian).
	videoHeaderLen = 6
	kindH264       = 1
	flagKeyframe   = 1
)

type inputMsg struct {
	T    string  `json:"t"`
	X    int     `json:"x"`
	Y    int     `json:"y"`
	B    int     `json:"b"`
	Down bool    `json:"down"`
	DX   float64 `json:"dx"`
	DY   float64 `json:"dy"`
	Code string  `json:"code"`
}

func (c *client) stream(ctx context.Context, sid string, width, height int) error {
	conn, err := c.dial(ctx, "/api/connector/stream?sid="+url.QueryEscape(sid))
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rc := newRateControl(width, height, time.Now())
	sess, err := platform.Start(platform.Options{Width: width, Height: height, Isolate: c.isolate(), Bitrate: rc.bitrate})
	if err != nil {
		sendJSON(ctx, conn, map[string]string{"t": "error", "message": err.Error()})
		conn.Close(websocket.StatusNormalClosure, "display setup failed")
		return err
	}
	defer sess.Close()
	defer sess.ReleaseAll()

	w, h := sess.Size()
	if err := sendJSON(ctx, conn, map[string]any{"t": "ready", "width": w, "height": h, "codec": "h264"}); err != nil {
		return err
	}

	credits := make(chan struct{}, maxInflight)
	for range maxInflight {
		credits <- struct{}{}
	}
	free := make(chan []byte, sendBuffers)
	for range sendBuffers {
		free <- nil
	}
	queued := make(chan []byte, 1)

	errc := make(chan error, 3)
	go func() { errc <- readInput(ctx, conn, sess, credits, rc) }()
	go func() { errc <- encodeFrames(ctx, sess, credits, free, queued) }()
	go func() { errc <- sendFrames(ctx, conn, free, queued, rc) }()
	err = <-errc
	cancel()
	conn.Close(websocket.StatusNormalClosure, "session ended")
	// All goroutines are done before ReleaseAll/Close touch the session.
	<-errc
	<-errc
	return err
}

func readInput(ctx context.Context, conn *websocket.Conn, sess platform.Session, credits chan struct{}, rc *rateControl) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var m inputMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "ack":
			if bps, changed := rc.acked(time.Now()); changed {
				sess.SetBitrate(bps)
			}
			select {
			case credits <- struct{}{}:
			default:
			}
		case "keyframe":
			sess.RequestKeyframe()
		case "move":
			err = sess.MouseMove(m.X, m.Y)
		case "button":
			if err = sess.MouseMove(m.X, m.Y); err == nil {
				err = sess.MouseButton(m.B, m.Down)
			}
		case "wheel":
			err = sess.Scroll(m.DX, m.DY)
		case "key":
			err = sess.Key(m.Code, m.Down)
		case "release":
			err = sess.ReleaseAll()
		}
		if err != nil {
			log.Printf("input %s: %v", m.T, err)
		}
	}
}

// encodeFrames pulls encoded frames while the viewer has room for them and hands them to
// sendFrames, so the next frame is captured and encoded while the previous one is in transit.
func encodeFrames(ctx context.Context, sess platform.Session, credits chan struct{}, free, queued chan []byte) error {
	var last time.Time
	for {
		select {
		case <-credits:
		case <-ctx.Done():
			return ctx.Err()
		}
		if wait := time.Until(last.Add(minFrameInterval)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		f, err := sess.NextFrame(ctx)
		if err != nil {
			return err
		}
		last = time.Now()
		var buf []byte
		select {
		case buf = <-free:
		case <-ctx.Done():
			return ctx.Err()
		}
		buf = appendVideoMessage(buf[:0], f)
		select {
		case queued <- buf:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func appendVideoMessage(buf []byte, f *platform.VideoFrame) []byte {
	var flags byte
	if f.Key {
		flags |= flagKeyframe
	}
	buf = append(buf, kindH264, flags)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(f.Width))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(f.Height))
	return append(buf, f.Data...)
}

func sendFrames(ctx context.Context, conn *websocket.Conn, free, queued chan []byte, rc *rateControl) error {
	for {
		var msg []byte
		select {
		case msg = <-queued:
		case <-ctx.Done():
			return ctx.Err()
		}
		rc.sent(len(msg), msg[1]&flagKeyframe != 0, time.Now())
		wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
		err := conn.Write(wctx, websocket.MessageBinary, msg)
		wcancel()
		if err != nil {
			return err
		}
		free <- msg
	}
}

func sendJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return errors.Join(errors.New("send failed"), err)
	}
	return nil
}
