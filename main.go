package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/emiago/sipgo/sip"
)

func main() {
	webAddr := flag.String("web", ":8212", "web interface listen address")
	sipAddr := flag.String("sip", "0.0.0.0:5060", "SIP UDP listen address")
	advertiseIP := flag.String("ip", "", "IP advertised in Contact/Via (auto-detected towards the SIP server if empty)")
	filesRoot := flag.String("root", ".", "directory exposed in the web file browser")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if os.Getenv("SIP_DEBUG") != "" { // full SIP message trace to stderr
		sip.SIPDebug = true
	}

	root, err := filepath.Abs(*filesRoot)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(root); err == nil && !fi.IsDir() {
			err = errors.New(root + " is not a directory")
		}
	}
	if err != nil {
		logger.Error("bad -root", "path", *filesRoot, "error", err)
		os.Exit(1)
	}
	logger.Info("file browser root", "path", root)

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
		Handler:           phone.HTTPHandler(root),
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
