package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// pair ties this machine to a GitHub account: the browser signs in on the portal, the user
// approves, and the portal redirects back to a loopback listener with a connector token.
func pair(ctx context.Context, server, name string) (token, machineID string, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	stateBytes := make([]byte, 24)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	type result struct {
		token, mid string
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state || q.Get("token") == "" || q.Get("mid") == "" {
			http.Error(w, "invalid pairing response", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>doremote</title>
<body style="font-family:system-ui;background:#0b0d12;color:#e6e8ee;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1>%s is connected</h1><p>You can close this tab and return to <a style="color:#7aa2ff" href="%s">doremote</a>.</p></div>`,
			html.EscapeString(name), html.EscapeString(server))
		select {
		case done <- result{q.Get("token"), q.Get("mid")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	u := server + "/connect?" + url.Values{
		"port":  {strconv.Itoa(port)},
		"state": {state},
		"name":  {name},
		"os":    {runtime.GOOS},
	}.Encode()
	log.Printf("opening browser to link this machine to your account:\n  %s", u)
	if err := openURL(u); err != nil {
		log.Printf("could not open a browser (%v); open the URL above manually", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	select {
	case r := <-done:
		// Let the success page flush before the listener closes.
		time.Sleep(200 * time.Millisecond)
		return r.token, r.mid, nil
	case <-ctx.Done():
		return "", "", errors.New("timed out waiting for browser sign-in")
	}
}

func openURL(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
