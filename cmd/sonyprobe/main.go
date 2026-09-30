// Command sonyprobe records what a Sony legacy Camera Remote API body supports:
// discovery, API lists in rec and movie mode, parameter candidates, optional
// record and zoom tests, and liveview statistics. Run it while joined to the
// camera's own AP; everything is written under -out/<body>/<timestamp>/.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := parseAndRun(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sonyprobe:", err)
		os.Exit(1)
	}
}

func parseAndRun(args []string) error {
	o := defaultOptions()
	fs := flag.NewFlagSet("sonyprobe", flag.ContinueOnError)
	fs.StringVar(&o.body, "body", "", "camera body name for the capture directory, e.g. rx10m4 (required)")
	fs.StringVar(&o.out, "out", o.out, "capture root directory")
	fs.StringVar(&o.endpoint, "endpoint", o.endpoint, "camera service URL used when SSDP or the device description fails")
	fs.BoolVar(&o.ssdp, "ssdp", o.ssdp, "discover the endpoint by SSDP first")
	fs.BoolVar(&o.wait, "wait", false, fmt.Sprintf("retry discovery and reachability for %v at start", o.waitFor))
	fs.BoolVar(&o.record, "record", false, "record a short clip: startMovieRec, hold, stopMovieRec")
	fs.BoolVar(&o.zoom, "zoom", false, "exercise actZoom in and out")
	fs.DurationVar(&o.liveview, "liveview", o.liveview, "read time per liveview session")
	fs.IntVar(&o.frames, "frames", o.frames, "JPEG frames saved per liveview session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.body == "" {
		fs.Usage()
		return errors.New("-body is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, o)
}
