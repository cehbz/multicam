// Package rig is the set of cameras the server controls, read from a config
// file.
package rig

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"

	"github.com/BurntSushi/toml"

	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/pixel"
	"github.com/cehbz/multicam/internal/sony"
)

// Rig is the cameras the server knows, in config order, each under its name
// and ready for the console.
type Rig struct {
	Cameras []console.Named
}

// Load reads the rig from the TOML config file at path. A Pixel's picture is
// shown on its side: its screenshots are portrait while it films on its side.
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
	for _, c := range cameras {
		cam, err := c.Kind.open()
		if err != nil {
			return nil, fmt.Errorf("%s: camera %s: %w", path, c.Name, err)
		}
		_, phone := c.Kind.(pixelPhone)
		rig.Cameras = append(rig.Cameras, console.Named{Name: c.Name, Camera: cam, OnItsSide: phone})
	}
	return rig, nil
}

// camera is one [[camera]] table of the config file: its name and, by its
// kind, how it is reached.
type camera struct {
	Name string
	Kind kind
}

// kind is what a camera's table says beyond its name; open makes the camera.
type kind interface {
	open() (console.Camera, error)
}

// sonyBody is a Sony body (kind "sony"): the network interface it is reached
// on (empty: the system's route) and its camera service endpoint
// (sony.DefaultEndpoint when left out).
type sonyBody struct {
	Interface string
	Endpoint  string
}

func (b sonyBody) open() (console.Camera, error) {
	cam, err := sony.NewCamera(b.Endpoint, b.Interface)
	if err != nil {
		return nil, err
	}
	return console.Adapt(cam), nil
}

// pixelPhone is a Pixel (kind "pixel"): the address of its wireless
// debugging, and the adb client that reaches it.
type pixelPhone struct {
	Address string
	adb     adbClient
}

func (p pixelPhone) open() (console.Camera, error) {
	return console.Adapt(pixel.NewCamera(pixel.ADB(p.adb.Path, p.adb.KeyDir, p.Address))), nil
}

// adbClient is the config's [adb] table: the adb binary and the directory it
// keeps its key in.
type adbClient struct {
	Path   string `toml:"path"`
	KeyDir string `toml:"key_dir"`
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
		ADB     adbClient        `toml:"adb"`
		Cameras []map[string]any `toml:"camera"`
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
			c.Kind = body
		case "pixel":
			phone := pixelPhone{Address: s.take("address"), adb: config.ADB}
			if _, _, err := net.SplitHostPort(phone.Address); err != nil {
				kindErr = fmt.Errorf("address %q is not the phone's wireless debugging address and port", phone.Address)
			} else if phone.adb.Path == "" || phone.adb.KeyDir == "" {
				kindErr = errors.New("a Pixel needs [adb] path and key_dir")
			}
			c.Kind = phone
		case "":
			kindErr = errors.New(`no kind: add kind = "sony" or kind = "pixel"`)
		default:
			kindErr = fmt.Errorf(`kind %q is not "sony" or "pixel"`, kindName)
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
