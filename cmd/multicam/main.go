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
	"sync"
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

// run serves the console for the rig's cameras on ln until ctx is done. The
// console's connection to the cameras starts as the rig's state file says,
// and it and requests end with ctx; run returns once the cameras' connections
// have ended, their liveview sessions closed. The camera links stay as they
// are. The rig's MediaMTX, if any, runs until ctx is done and is ended before
// run returns.
func run(ctx context.Context, ln net.Listener, r *rig.Rig) error {
	var names []string
	for _, c := range r.Cameras {
		names = append(names, c.Name)
	}
	cons := console.New(ctx, r.Cameras, r.State)
	srv := &http.Server{
		Handler:     cons,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	var media sync.WaitGroup
	if r.MediaMTX != nil {
		media.Go(func() {
			if err := r.MediaMTX.Run(ctx); err != nil {
				slog.Error("mediamtx", "error", err)
			}
		})
	}
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		stopped <- srv.Shutdown(context.WithoutCancel(ctx))
	}()
	slog.Info("console", "url", "http://"+ln.Addr().String()+"/", "cameras", names)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	err := <-stopped
	cons.Wait()
	media.Wait()
	return err
}
