package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// errRevoked means the portal no longer recognises this machine's token.
var errRevoked = errors.New("machine was removed from your account")

type controlMsg struct {
	T      string `json:"t"`
	SID    string `json:"sid,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type client struct {
	server  string // https://doremote.dev
	token   string
	name    string
	isolate func() bool  // read per session so the setting can change while running
	status  func(string) // user-visible connection state

	mu      sync.Mutex
	current *activeSession
}

type activeSession struct {
	sid    string
	cancel context.CancelFunc
	done   chan struct{}
}

func wsURL(server, path string) string {
	switch {
	case strings.HasPrefix(server, "https://"):
		return "wss://" + strings.TrimPrefix(server, "https://") + path
	case strings.HasPrefix(server, "http://"):
		return "ws://" + strings.TrimPrefix(server, "http://") + path
	}
	return server + path
}

func (c *client) dial(ctx context.Context, path string) (*websocket.Conn, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	conn, resp, err := websocket.Dial(ctx, wsURL(c.server, path), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, errRevoked
		}
		return nil, err
	}
	return conn, nil
}

// run keeps the control channel up, reconnecting with backoff until ctx ends or the token is revoked.
func (c *client) run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := c.control(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRevoked) {
			return err
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		log.Printf("disconnected: %v; reconnecting in %s", err, backoff)
		c.status("Offline — reconnecting…")
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *client) control(ctx context.Context) error {
	conn, err := c.dial(ctx, "/api/connector/ws")
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hello, _ := json.Marshal(map[string]string{"t": "hello", "name": c.name, "os": runtime.GOOS, "version": version})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		return err
	}
	log.Printf("online as %q — open %s to connect", c.name, c.server)
	c.status("Online — ready for remote sessions")

	// Keepalive: the hub answers {"t":"ping"} without waking up.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, []byte(`{"t":"ping"}`))
				wcancel()
				if err != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == 4401 {
				return errRevoked
			}
			if websocket.CloseStatus(err) == 4409 {
				return fmt.Errorf("another connector instance took over this machine: %w", err)
			}
			return err
		}
		var m controlMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "session":
			c.startSession(ctx, m)
		case "end":
			c.endSession(m.SID)
		}
	}
}

// startSession replaces any running session; only one virtual display exists at a time.
func (c *client) startSession(ctx context.Context, m controlMsg) {
	c.mu.Lock()
	prev := c.current
	sctx, cancel := context.WithCancel(ctx)
	s := &activeSession{sid: m.SID, cancel: cancel, done: make(chan struct{})}
	c.current = s
	c.mu.Unlock()

	go func() {
		defer close(s.done)
		if prev != nil {
			prev.cancel()
			<-prev.done
		}
		log.Printf("session %s: starting %dx%d", m.SID, m.Width, m.Height)
		c.status(fmt.Sprintf("In a remote session (%d×%d)", m.Width, m.Height))
		err := c.stream(sctx, m.SID, m.Width, m.Height)
		log.Printf("session %s: ended (%v)", m.SID, err)
		if ctx.Err() == nil {
			c.status("Online — ready for remote sessions")
		}
		c.mu.Lock()
		if c.current == s {
			c.current = nil
		}
		c.mu.Unlock()
	}()
}

func (c *client) endSession(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && c.current.sid == sid {
		c.current.cancel()
	}
}

// inSession reports whether a remote session is running (or still restoring displays).
func (c *client) inSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current != nil
}

// shutdown cancels the running session and waits for its displays to be restored.
func (c *client) shutdown() {
	c.mu.Lock()
	s := c.current
	c.mu.Unlock()
	if s != nil {
		s.cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			fmt.Fprintln(os.Stderr, "timed out restoring displays")
		}
	}
}
