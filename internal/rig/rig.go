// Package rig is the set of cameras the server controls, read from a config
// file.
package rig

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/BurntSushi/toml"

	"github.com/cehbz/multicam/internal/sony"
)

// Camera is one of the rig's cameras.
type Camera struct {
	Name string
	*sony.Camera
}

// Rig is the cameras the server knows, in config order.
type Rig struct {
	Cameras []Camera
}

// cameraConfig is one [[camera]] table of the config file: the camera's name,
// the network interface it is reached on (empty: the system's route) and its
// camera service endpoint (sony.DefaultEndpoint when left out).
type cameraConfig struct {
	Name      string `toml:"name"`
	Interface string `toml:"interface"`
	Endpoint  string `toml:"endpoint"`
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
	for _, c := range cameras {
		body, err := sony.NewCamera(c.Endpoint, c.Interface)
		if err != nil {
			return nil, fmt.Errorf("%s: camera %s: %w", path, c.Name, err)
		}
		rig.Cameras = append(rig.Cameras, Camera{Name: c.Name, Camera: body})
	}
	return rig, nil
}

// A camera's name is one URL path segment.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// parse decodes and validates a config: at least one camera, each with a
// unique valid name.
func parse(text string) ([]cameraConfig, error) {
	var config struct {
		Cameras []cameraConfig `toml:"camera"`
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
	numbers := map[string]int{}
	for i := range config.Cameras {
		c := &config.Cameras[i]
		if !validName.MatchString(c.Name) {
			return nil, fmt.Errorf("camera %d: name %q is not letters, digits, - and _", i+1, c.Name)
		}
		if first, dup := numbers[c.Name]; dup {
			return nil, fmt.Errorf("camera %d: name %q is already camera %d", i+1, c.Name, first)
		}
		numbers[c.Name] = i + 1
		if c.Endpoint == "" {
			c.Endpoint = sony.DefaultEndpoint
		}
	}
	return config.Cameras, nil
}
