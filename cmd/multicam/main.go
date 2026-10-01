// Command multicam serves the rig's console for the camera at the fixed Sony
// endpoint.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/sony"
)

// addr is where the console listens.
const addr = "localhost:8080"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		err = run(ctx, ln, sony.DefaultEndpoint)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "multicam:", err)
		os.Exit(1)
	}
}

// run serves the console for the camera at endpoint on ln until ctx is done.
// Requests end with ctx, and run returns once their liveview sessions have
// closed.
func run(ctx context.Context, ln net.Listener, endpoint string) error {
	srv := &http.Server{
		Handler:     console.New(sony.NewCamera(endpoint).Liveview),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		stopped <- srv.Shutdown(context.WithoutCancel(ctx))
	}()
	slog.Info("console", "url", "http://"+ln.Addr().String()+"/", "camera", endpoint)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return <-stopped
}
