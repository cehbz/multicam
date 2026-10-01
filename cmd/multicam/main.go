// Command multicam serves the console for the rig of cameras in its config
// file: multicam [config.toml], multicam.toml in the working directory when
// not given.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/rig"
)

// addr is where the console listens.
const addr = "localhost:8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := start(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "multicam:", err)
		os.Exit(1)
	}
}

func start(ctx context.Context, args []string) error {
	path, err := configPath(args)
	if err != nil {
		return err
	}
	r, err := rig.Load(path)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return run(ctx, ln, r)
}

// configPath is the config file the arguments name: the one optional
// argument, or multicam.toml in the working directory.
func configPath(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "multicam.toml", nil
	case 1:
		return args[0], nil
	}
	return "", errors.New("usage: multicam [config.toml]")
}

// run serves the console for the rig's first camera on ln until ctx is done.
// Requests end with ctx, and run returns once their liveview sessions have
// closed.
func run(ctx context.Context, ln net.Listener, r *rig.Rig) error {
	cam := r.Cameras[0]
	srv := &http.Server{
		Handler:     console.New(cam),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		stopped <- srv.Shutdown(context.WithoutCancel(ctx))
	}()
	slog.Info("console", "url", "http://"+ln.Addr().String()+"/", "camera", cam.Name)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return <-stopped
}
