package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/joey/doremote/connector/platform"
)

// maxInflight bounds frames sent but not yet acknowledged by the viewer, which keeps latency
// low on slow links instead of queueing stale frames.
const maxInflight = 2

const minFrameInterval = time.Second / 30

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

	sess, err := platform.Start(platform.Options{Width: width, Height: height, Isolate: c.isolate()})
	if err != nil {
		sendJSON(ctx, conn, map[string]string{"t": "error", "message": err.Error()})
		conn.Close(websocket.StatusNormalClosure, "display setup failed")
		return err
	}
	defer sess.Close()
	defer sess.ReleaseAll()

	w, h := sess.Size()
	if err := sendJSON(ctx, conn, map[string]any{"t": "ready", "width": w, "height": h}); err != nil {
		return err
	}

	credits := make(chan struct{}, maxInflight)
	for range maxInflight {
		credits <- struct{}{}
	}

	errc := make(chan error, 2)
	go func() { errc <- readInput(ctx, conn, sess, credits) }()
	go func() { errc <- sendFrames(ctx, conn, sess, credits) }()
	err = <-errc
	cancel()
	conn.Close(websocket.StatusNormalClosure, "session ended")
	<-errc // both goroutines are done before ReleaseAll/Close touch the session
	return err
}

func readInput(ctx context.Context, conn *websocket.Conn, sess platform.Session, credits chan struct{}) error {
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
			select {
			case credits <- struct{}{}:
			default:
			}
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

func sendFrames(ctx context.Context, conn *websocket.Conn, sess platform.Session, credits chan struct{}) error {
	var enc frameEncoder
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
		var msg []byte
		for msg == nil {
			f, err := sess.NextFrame(ctx)
			if err != nil {
				return err
			}
			// nil means the capture API reported a frame identical to the last one sent.
			if msg, err = enc.encode(f); err != nil {
				return err
			}
		}
		last = time.Now()
		wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
		err := conn.Write(wctx, websocket.MessageBinary, msg)
		wcancel()
		if err != nil {
			return err
		}
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
