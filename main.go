package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emiago/sipgo/sip"
)

func main() {
	webAddr := flag.String("web", ":8212", "web interface listen address")
	sipAddr := flag.String("sip", "0.0.0.0:5060", "SIP UDP listen address")
	advertiseIP := flag.String("ip", "", "IP advertised in Contact/Via (auto-detected towards the SIP server if empty)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if os.Getenv("SIP_DEBUG") != "" { // full SIP message trace to stderr
		sip.SIPDebug = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	phone, err := NewPhone(*sipAddr, *advertiseIP, logger)
	if err != nil {
		logger.Error("failed to init phone", "error", err)
		os.Exit(1)
	}

	if err := phone.Start(ctx, *sipAddr); err != nil {
		logger.Error("failed to start SIP stack", "error", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              *webAddr,
		Handler:           phone.HTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("web interface listening", "addr", *webAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("web server failed", "error", err)
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}
}
