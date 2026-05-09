package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/directory"
)

// RunDirectory runs the local A2A discovery service.
func RunDirectory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("directory", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr("A2A_DIRECTORY_ADDR", "127.0.0.1:7777"), "listen address")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge directory [flags]\n\nRun the local discovery service that bridges register with.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	log := slog.New(slog.NewJSONHandler(stdout, nil))
	reg := directory.New(log)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           reg.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("directory listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			log.Error("listen", "err", err)
			return 1
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
