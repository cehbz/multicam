// Package rig is the set of cameras the server controls, read from a config
// file.
package rig

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cehbz/multicam/internal/blackmagic"
	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/mediamtx"
	"github.com/cehbz/multicam/internal/sony"
)

// Rig is the cameras the server knows, in config order, each under its name
// and ready for the console, and the MediaMTX to run beside them (nil when
// the config names none).
type Rig struct {
	Cameras  []console.Named
	MediaMTX *mediamtx.Server
}

// Load reads the rig from the TOML config file at path.
func Load(path string) (*Rig, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cameras, err := parse(string(text))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rig := &Rig{}
	if rig.MediaMTX, err = parseMediaMTX(string(text)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, c := range cameras {
		cam, err := c.Kind.open(c.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: camera %s: %w", path, c.Name, err)
		}
		rig.Cameras = append(rig.Cameras, cam)
	}
	return rig, nil
}

// camera is one [[camera]] table of the config file: its name and, by its
// kind, how it is reached.
type camera struct {
	Name string
	Kind kind
}

// kind is what a camera's table says beyond its name; open makes the camera
// under name.
type kind interface {
	open(name string) (console.Named, error)
}

// sonyBody is a Sony body (kind "sony"): the network interface it is reached
// on (empty: the system's route), its camera service endpoint
// (sony.DefaultEndpoint when left out) and the gap a start keeps after the
// body reports IDLE (start_gap, a duration; none when left out). The console
// relays its liveview.
type sonyBody struct {
	Interface string
	Endpoint  string
	StartGap  time.Duration
}

func (b sonyBody) open(name string) (console.Named, error) {
	cam, err := sony.NewCamera(b.Endpoint, b.Interface)
	if err != nil {
		return console.Named{}, err
	}
	cam.StartGap = b.StartGap
	return console.Named{Name: name, Picture: console.Relay(cam), Camera: sonyCamera{cam}}, nil
}

// sonyCamera is a Sony body as the console watches it: recording while its
// status is MovieRecording.
type sonyCamera struct{ *sony.Camera }

func (c sonyCamera) Watch(ctx context.Context) (<-chan bool, error) {
	statuses, err := c.Camera.Watch(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan bool)
	go func() {
		defer close(ch)
		for s := range statuses {
			select {
			case ch <- s == "MovieRecording":
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// blackmagicPhone is a phone running Blackmagic Camera (kind "blackmagic"):
// the address of the app's HTTP server. Its picture is the app's livestream,
// which the page plays from MediaMTX under the camera's name.
type blackmagicPhone struct {
	Address string
}

func (p blackmagicPhone) open(name string) (console.Named, error) {
	cam := blackmagic.NewCamera(p.Address)
	return console.Named{Name: name, Picture: console.Streamed{Path: name}, Camera: cam}, nil
}

// settings is the keys of one [[camera]] table. take removes the ones read;
// the ones left are unknown to the camera's kind.
type settings struct {
	keys map[string]any
	err  error // the first value that was not a string
}

// take returns the string at key, empty when the table lacks it.
func (s *settings) take(key string) string {
	v, present := s.keys[key]
	delete(s.keys, key)
	str, isString := v.(string)
	if present && !isString && s.err == nil {
		s.err = fmt.Errorf("%s is not a string", key)
	}
	return str
}

// A camera's name is one URL path segment.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// parse decodes and validates a config: at least one camera, each with a
// unique valid name and the settings of its kind.
func parse(text string) ([]camera, error) {
	var config struct {
		Cameras  []map[string]any `toml:"camera"`
		MediaMTX map[string]any   `toml:"mediamtx"` // read by parseMediaMTX
	}
	md, err := toml.Decode(text, &config)
	if err != nil {
		return nil, err
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("unknown key %s", unknown[0])
	}
	if len(config.Cameras) == 0 {
		return nil, errors.New("no cameras")
	}
	var cameras []camera
	numbers := map[string]int{}
	for i, keys := range config.Cameras {
		s := &settings{keys: keys}
		name := s.take("name")
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("camera %d: name %q is not letters, digits, - and _", i+1, name)
		}
		if first, dup := numbers[name]; dup {
			return nil, fmt.Errorf("camera %d: name %q is already camera %d", i+1, name, first)
		}
		numbers[name] = i + 1

		c := camera{Name: name}
		var kindErr error
		switch kindName := s.take("kind"); kindName {
		case "sony":
			body := sonyBody{Interface: s.take("interface"), Endpoint: s.take("endpoint")}
			if body.Endpoint == "" {
				body.Endpoint = sony.DefaultEndpoint
			}
			if gap := s.take("start_gap"); gap != "" {
				d, err := time.ParseDuration(gap)
				if err != nil {
					kindErr = fmt.Errorf("start_gap %q is not a duration", gap)
				}
				body.StartGap = d
			}
			c.Kind = body
		case "blackmagic":
			phone := blackmagicPhone{Address: s.take("address")}
			if _, _, err := net.SplitHostPort(phone.Address); err != nil {
				kindErr = fmt.Errorf("address %q is not the phone's HTTP server address and port", phone.Address)
			}
			c.Kind = phone
		case "":
			kindErr = errors.New(`no kind: add kind = "sony" or kind = "blackmagic"`)
		default:
			kindErr = fmt.Errorf(`kind %q is not "sony" or "blackmagic"`, kindName)
		}
		if kindErr == nil {
			kindErr = s.err
		}
		if unknown := slices.Sorted(maps.Keys(s.keys)); kindErr == nil && len(unknown) > 0 {
			kindErr = fmt.Errorf("unknown key %q", unknown[0])
		}
		if kindErr != nil {
			return nil, fmt.Errorf("camera %d (%s): %w", i+1, name, kindErr)
		}
		cameras = append(cameras, c)
	}
	return cameras, nil
}

// mediaMTXRestartDelay is the wait before a MediaMTX that exited is started
// again.
const mediaMTXRestartDelay = time.Second

// parseMediaMTX reads the config's [mediamtx] table: path (the executable,
// required), dir (its working directory, the executable's directory when left
// out) and log (its output file, mediamtx.log in dir when left out). It
// returns nil when the config has no such table.
func parseMediaMTX(text string) (*mediamtx.Server, error) {
	var config struct {
		MediaMTX map[string]any `toml:"mediamtx"`
	}
	if _, err := toml.Decode(text, &config); err != nil {
		return nil, err
	}
	if config.MediaMTX == nil {
		return nil, nil
	}
	s := &settings{keys: config.MediaMTX}
	path, dir, log := s.take("path"), s.take("dir"), s.take("log")
	if s.err == nil {
		if unknown := slices.Sorted(maps.Keys(s.keys)); len(unknown) > 0 {
			s.err = fmt.Errorf("unknown key %q", unknown[0])
		}
	}
	if s.err != nil {
		return nil, fmt.Errorf("mediamtx: %w", s.err)
	}
	if path == "" {
		return nil, errors.New("mediamtx: no path")
	}
	if dir == "" {
		dir = filepath.Dir(path)
	}
	if log == "" {
		log = filepath.Join(dir, "mediamtx.log")
	}
	return &mediamtx.Server{Path: path, Dir: dir, Log: log, RestartDelay: mediaMTXRestartDelay}, nil
}
