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
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cehbz/multicam/internal/blackmagic"
	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/link"
	"github.com/cehbz/multicam/internal/mediamtx"
	"github.com/cehbz/multicam/internal/sony"
)

// Rig is the cameras the server knows, in config order, each under its name
// and ready for the console, the MediaMTX to run beside them (nil when the
// config names none) and the file that keeps the connection state.
type Rig struct {
	Cameras  []console.Named
	MediaMTX *mediamtx.Server
	State    console.StateFile
}

// stateFile is the state file's name beside the config file, where it is
// kept unless the config names another.
const stateFile = "connection.state"

// Load reads the rig from the TOML config file at path. The links' directory
// defaults to the config file's directory.
func Load(path string) (*Rig, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parse(string(text))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if cfg.state == "" {
		cfg.state = filepath.Join(dir, stateFile)
	}
	if cfg.links.Dir == "" {
		cfg.links.Dir = dir
	}
	rig := &Rig{MediaMTX: cfg.mediaMTX, State: console.StateFile(cfg.state)}
	keeper := sync.OnceValues(func() (*link.Keeper, error) { return link.New(cfg.links) })
	for _, c := range cfg.cameras {
		cam, err := c.Kind.open(c.Name, keeper)
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
// under name, its link, if it has one, kept by the keeper keeper returns.
type kind interface {
	open(name string, keeper func() (*link.Keeper, error)) (console.Named, error)
}

// firstTable is the routing table of the first Sony body on an interface;
// each next one's is one more.
const firstTable = 2001

// sonyBody is a Sony body (kind "sony"): the network interface it is joined
// on (empty: reached over the system's route, with no link of its own), the
// body's network name and password, which a body on an interface needs, its
// camera service endpoint (sony.DefaultEndpoint when left out) and the gap a
// start keeps after the body reports IDLE (start_gap, a duration; none when
// left out). Its link's route is in Table, one per body on an interface. The
// console relays its liveview.
type sonyBody struct {
	Interface string
	Endpoint  string
	StartGap  time.Duration
	Network   string
	Password  string
	Table     int
}

func (b sonyBody) open(name string, keeper func() (*link.Keeper, error)) (console.Named, error) {
	cam, err := sony.NewCamera(b.Endpoint, b.Interface)
	if err != nil {
		return console.Named{}, err
	}
	cam.StartGap = b.StartGap
	named := console.Named{Name: name, Picture: console.Relay(cam), Camera: sonyCamera{cam}}
	if b.Interface != "" {
		k, err := keeper()
		if err != nil {
			return console.Named{}, err
		}
		named.Link = sonyLink{keeper: k, config: link.Config{Interface: b.Interface, Network: b.Network, Password: b.Password, Table: b.Table}}
	}
	return named, nil
}

// keeper joins and leaves the Sony bodies' links, as a *link.Keeper does.
type keeper interface {
	Join(ctx context.Context, c link.Config) (*link.Link, error)
	Leave(ctx context.Context, iface string) error
}

// sonyLink is a Sony body's link, joined and left by its keeper.
type sonyLink struct {
	keeper keeper
	config link.Config
}

func (l sonyLink) Join(ctx context.Context) (console.Joined, error) {
	j, err := l.keeper.Join(ctx, l.config)
	if err != nil {
		return nil, err
	}
	return j, nil
}

func (l sonyLink) Leave(ctx context.Context) error { return l.keeper.Leave(ctx, l.config.Interface) }

// watch is a camera's watch as the console takes it: each delivery of
// source's watch as status gives it, until that watch ends or ctx does.
func watch[T any](ctx context.Context, source func(context.Context) (<-chan T, error), status func(T) console.Status) (<-chan console.Status, error) {
	deliveries, err := source(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan console.Status)
	go func() {
		defer close(ch)
		for d := range deliveries {
			select {
			case ch <- status(d):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// sonyCamera is a Sony body as the console watches it: recording while its
// status is MovieRecording, its picture, which the console relays, playable
// whenever it is watched.
type sonyCamera struct{ *sony.Camera }

func (c sonyCamera) Watch(ctx context.Context) (<-chan console.Status, error) {
	return watch(ctx, c.Camera.Watch, func(s sony.Status) console.Status {
		return console.Status{Recording: s == "MovieRecording", Picture: true}
	})
}

// blackmagicCamera is a phone running Blackmagic Camera as the console
// watches it: its picture playable while its livestream is streaming.
type blackmagicCamera struct{ *blackmagic.Camera }

func (c blackmagicCamera) Watch(ctx context.Context) (<-chan console.Status, error) {
	return watch(ctx, c.Camera.Watch, func(s blackmagic.State) console.Status {
		return console.Status{Recording: s.Recording, Picture: s.Streaming}
	})
}

// blackmagicPhone is a phone running Blackmagic Camera (kind "blackmagic"):
// the address of the app's HTTP server. Its picture is the app's livestream,
// which the page plays from MediaMTX under the camera's name.
type blackmagicPhone struct {
	Address string
}

func (p blackmagicPhone) open(name string, _ func() (*link.Keeper, error)) (console.Named, error) {
	cam := blackmagic.NewCamera(p.Address)
	return console.Named{Name: name, Picture: console.Streamed{Path: name}, Camera: blackmagicCamera{cam}}, nil
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

// config is a config file's contents.
type config struct {
	cameras  []camera
	mediaMTX *mediamtx.Server // nil when the file names none
	links    link.Options     // the [link] table's
	state    string           // the state file; empty when not named
}

// parse decodes and validates a config: at least one camera, each with a
// unique valid name and the settings of its kind, the [mediamtx] table, the
// [link] table (supplicant, lib, dir and phy, each optional) and the state
// file (state).
func parse(text string) (config, error) {
	var file struct {
		State string `toml:"state"`
		Link  struct {
			Supplicant string `toml:"supplicant"`
			Lib        string `toml:"lib"`
			Dir        string `toml:"dir"`
			Phy        string `toml:"phy"`
		} `toml:"link"`
		MediaMTX *mediaMTXTable   `toml:"mediamtx"`
		Cameras  []map[string]any `toml:"camera"`
	}
	md, err := toml.Decode(text, &file)
	if err != nil {
		return config{}, err
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		return config{}, fmt.Errorf("unknown key %s", unknown[0])
	}
	cfg := config{links: link.Options(file.Link), state: file.State}
	if cfg.mediaMTX, err = file.MediaMTX.server(); err != nil {
		return config{}, err
	}
	if cfg.cameras, err = parseCameras(file.Cameras); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseCameras validates the [[camera]] tables.
func parseCameras(tables []map[string]any) ([]camera, error) {
	if len(tables) == 0 {
		return nil, errors.New("no cameras")
	}
	var cameras []camera
	numbers := map[string]int{}
	interfaces := map[string]int{} // the camera number of each body's interface
	for i, keys := range tables {
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
			body := sonyBody{Interface: s.take("interface"), Endpoint: s.take("endpoint"), Network: s.take("network"), Password: s.take("password")}
			if body.Endpoint == "" {
				body.Endpoint = sony.DefaultEndpoint
			}
			switch {
			case body.Interface != "" && (body.Network == "" || body.Password == ""):
				kindErr = errors.New("a body on an interface needs its network and password")
			case body.Interface == "" && (body.Network != "" || body.Password != ""):
				kindErr = errors.New("network and password need an interface")
			case body.Interface != "":
				if first, dup := interfaces[body.Interface]; dup {
					kindErr = fmt.Errorf("interface %q is already camera %d's", body.Interface, first)
				}
				interfaces[body.Interface] = i + 1
				body.Table = firstTable + len(interfaces) - 1
			}
			if gap := s.take("start_gap"); gap != "" {
				d, err := time.ParseDuration(gap)
				if err != nil && kindErr == nil {
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

// mediaMTXTable is the config's [mediamtx] table: path (the executable,
// required), dir (its working directory, the executable's directory when left
// out) and log (its output file, mediamtx.log in dir when left out).
type mediaMTXTable struct {
	Path string `toml:"path"`
	Dir  string `toml:"dir"`
	Log  string `toml:"log"`
}

// server is the MediaMTX t names; nil without the table.
func (t *mediaMTXTable) server() (*mediamtx.Server, error) {
	if t == nil {
		return nil, nil
	}
	if t.Path == "" {
		return nil, errors.New("mediamtx: no path")
	}
	dir, log := t.Dir, t.Log
	if dir == "" {
		dir = filepath.Dir(t.Path)
	}
	if log == "" {
		log = filepath.Join(dir, "mediamtx.log")
	}
	return &mediamtx.Server{Path: t.Path, Dir: dir, Log: log, RestartDelay: mediaMTXRestartDelay}, nil
}
