// Command live-worker is the live worker service: a supervisor that owns N
// warm worker processes (live-worker-slot), one per encode slot, and makes
// the segments of live rungs for a stream origin over HTTP. It never links
// the media libraries itself; a crash in them ends one worker process, which
// is started again while its job is made elsewhere.
//
// Configuration is environment-driven (see the README).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zaentrum/live-worker/internal/config"
	"github.com/zaentrum/live-worker/internal/supervisor"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	s, err := supervisor.New(cfg.Supervisor)
	if err != nil {
		log.Fatalf("supervisor: %v", err)
	}
	if err := s.Start(); err != nil {
		log.Fatalf("supervisor: %v", err)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("live-worker listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	<-ctx.Done()
	log.Print("shutting down")
	// The supervisor first: it stops taking jobs and lets the running ones
	// finish, while the server still answers them.
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	sctx, cancel := context.WithTimeout(context.Background(), cfg.Supervisor.ShutdownGrace+10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-done
}
