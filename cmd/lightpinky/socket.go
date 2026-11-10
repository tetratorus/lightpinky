package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveSocket serves MCP on a Unix socket only this user can reach, so the
// phones it opened (and a Simulator's WebDriverAgent run) stay open across
// short-lived clients. It returns after idle without requests, or on
// SIGINT/SIGTERM, having closed the phones.
func serveSocket(srv *sdk.Server, path string, idle time.Duration, log *slog.Logger, closeAll func()) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The directory, not the socket's own mode, keeps other users out.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return errors.New(path + " is already served")
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var mu sync.Mutex
	busy, last := 0, time.Now()
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		busy++
		mu.Unlock()
		defer func() {
			mu.Lock()
			busy--
			last = time.Now()
			mu.Unlock()
		}()
		h.ServeHTTP(w, r)
	}))
	hs := &http.Server{Handler: mux}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		var tick <-chan time.Time
		if idle > 0 {
			t := time.NewTicker(min(idle/4, 15*time.Second) + time.Millisecond)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-stop:
				log.Info("stopping")
				hs.Shutdown(context.Background())
				return
			case <-tick:
				mu.Lock()
				done := busy == 0 && time.Since(last) > idle
				mu.Unlock()
				if done {
					log.Info("idle, stopping", "after", idle)
					hs.Shutdown(context.Background())
					return
				}
			}
		}
	}()
	log.Info("serving MCP on a Unix socket", "path", path, "idle", idle)
	err = hs.Serve(l)
	closeAll()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
