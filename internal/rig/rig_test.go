package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cehbz/multicam/internal/sony"
	"github.com/cehbz/multicam/internal/sony/sonytest"
)

func TestParse(t *testing.T) {
	adb := adbClient{Path: "/usr/bin/adb", KeyDir: "/var/adbhome"}
	const adbTable = "[adb]\npath = \"/usr/bin/adb\"\nkey_dir = \"/var/adbhome\"\n"
	const pixelTable = "[[camera]]\nname = \"pixel9\"\nkind = \"pixel\"\naddress = \"192.168.1.109:41419\"\n"
	tests := []struct {
		name    string
		text    string
		want    []camera
		wantErr string // part of the error; empty when the config is valid
	}{
		{
			name: "two Sony bodies on their interfaces",
			text: "[[camera]]\nname = \"rx10m4\"\nkind = \"sony\"\ninterface = \"wlan1\"\n[[camera]]\nname = \"rx100m6\"\nkind = \"sony\"\ninterface = \"cam2\"\n",
			want: []camera{{"rx10m4", sonyBody{"wlan1", sony.DefaultEndpoint}}, {"rx100m6", sonyBody{"cam2", sony.DefaultEndpoint}}},
		},
		{
			name: "Sony body with an endpoint and no interface",
			text: "[[camera]]\nname = \"fake-1\"\nkind = \"sony\"\nendpoint = \"http://127.0.0.1:9/sony/camera\"\n",
			want: []camera{{"fake-1", sonyBody{"", "http://127.0.0.1:9/sony/camera"}}},
		},
		{
			name: "a Pixel between two Sony bodies, adb named last",
			text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\n" + pixelTable + "[[camera]]\nname = \"b\"\nkind = \"sony\"\ninterface = \"cam2\"\n" + adbTable,
			want: []camera{
				{"a", sonyBody{"", sony.DefaultEndpoint}},
				{"pixel9", pixelPhone{"192.168.1.109:41419", adb}},
				{"b", sonyBody{"cam2", sony.DefaultEndpoint}},
			},
		},
		{name: "no cameras", text: "", wantErr: "no cameras"},
		{name: "camera without a name", text: "[[camera]]\nkind = \"sony\"\n", wantErr: "camera 1"},
		{name: "empty name", text: "[[camera]]\nname = \"\"\nkind = \"sony\"\n", wantErr: "camera 1"},
		{name: "name with a slash", text: "[[camera]]\nname = \"rx/10\"\nkind = \"sony\"\n", wantErr: `"rx/10"`},
		{name: "name with a space", text: "[[camera]]\nname = \"rx 10\"\nkind = \"sony\"\n", wantErr: `"rx 10"`},
		{name: "name of dots", text: "[[camera]]\nname = \"..\"\nkind = \"sony\"\n", wantErr: `".."`},
		{
			name:    "duplicate name",
			text:    "[[camera]]\nname = \"a\"\nkind = \"sony\"\n[[camera]]\nname = \"b\"\nkind = \"sony\"\n" + adbTable + "[[camera]]\nname = \"a\"\nkind = \"pixel\"\naddress = \"1.2.3.4:5\"\n",
			wantErr: `camera 3: name "a" is already camera 1`,
		},
		{
			name:    "no kind",
			text:    "[[camera]]\nname = \"rx10m4\"\ninterface = \"wlan1\"\n",
			wantErr: `camera 1 (rx10m4): no kind: add kind = "sony" or kind = "pixel"`,
		},
		{name: "unknown kind", text: "[[camera]]\nname = \"a\"\nkind = \"gopro\"\n", wantErr: `camera 1 (a): kind "gopro" is not "sony" or "pixel"`},
		{name: "Pixel without an address", text: adbTable + "[[camera]]\nname = \"pixel9\"\nkind = \"pixel\"\n", wantErr: "camera 1 (pixel9): address"},
		{
			name:    "Pixel address without a port",
			text:    adbTable + "[[camera]]\nname = \"pixel9\"\nkind = \"pixel\"\naddress = \"192.168.1.109\"\n",
			wantErr: `camera 1 (pixel9): address "192.168.1.109"`,
		},
		{name: "Pixel without the adb settings", text: pixelTable, wantErr: "camera 1 (pixel9): a Pixel needs [adb] path and key_dir"},
		{
			name:    "Pixel with half the adb settings",
			text:    "[adb]\npath = \"/usr/bin/adb\"\n" + pixelTable,
			wantErr: "camera 1 (pixel9): a Pixel needs [adb] path and key_dir",
		},
		{
			name:    "misspelled key",
			text:    "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninteface = \"wlan1\"\n",
			wantErr: `camera 1 (a): unknown key "inteface"`,
		},
		{
			name:    "another kind's key",
			text:    adbTable + "[[camera]]\nname = \"pixel9\"\nkind = \"pixel\"\naddress = \"1.2.3.4:5\"\ninterface = \"wlan1\"\n",
			wantErr: `camera 1 (pixel9): unknown key "interface"`,
		},
		{name: "key that is not a string", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninterface = 1\n", wantErr: "camera 1 (a): interface"},
		{name: "unknown top-level key", text: "abd = 1\n[[camera]]\nname = \"a\"\nkind = \"sony\"\n", wantErr: "unknown key abd"},
		{name: "unknown adb key", text: "[adb]\nhome = \"/x\"\n[[camera]]\nname = \"a\"\nkind = \"sony\"\n", wantErr: "unknown key adb.home"},
		{name: "not TOML", text: "[[camera]\n", wantErr: "toml: line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse = %v, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("parse = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

const phoneConfig = "../../multicam.phone.toml"

func TestPhoneConfig(t *testing.T) {
	text, err := os.ReadFile(phoneConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse(string(text))
	want := []camera{
		{"rx10m4", sonyBody{"wlan1", sony.DefaultEndpoint}},
		{"rx100m6", sonyBody{"cam2", sony.DefaultEndpoint}},
		{"pixel9", pixelPhone{"192.168.1.109:41419", adbClient{"/data/data/com.termux/files/usr/bin/adb", "/data/local/tmp/mc/adbhome"}}},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("parse = %v, %v; want %v", got, err, want)
	}
}

func TestLoadGivesEachCameraItsOwnBody(t *testing.T) {
	first, second := sonytest.NewCamera(t), sonytest.NewCamera(t)
	path := filepath.Join(t.TempDir(), "multicam.toml")
	config := fmt.Sprintf("[[camera]]\nname = \"first\"\nkind = \"sony\"\nendpoint = %q\n[[camera]]\nname = \"second\"\nkind = \"sony\"\nendpoint = %q\n",
		first.Endpoint(), second.Endpoint())
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	rig, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rig.Cameras) != 2 || rig.Cameras[0].Name != "first" || rig.Cameras[1].Name != "second" {
		t.Fatalf("rig cameras %v, want first and second", rig.Cameras)
	}
	if err := rig.Cameras[1].StartRecording(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Cameras[0].Recording(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := first.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls %v, want %v", got, want)
	}
	if got, want := second.Calls(), []string{"startMovieRec@1.0"}; !slices.Equal(got, want) {
		t.Errorf("second camera's calls %v, want %v", got, want)
	}
}

func TestLoadErrorsNameTheFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "multicam.toml")
	if rig, err := Load(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("Load of a missing file = %v, %v; want an error naming it", rig, err)
	}
	invalid := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(invalid, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rig, err := Load(invalid); err == nil || !strings.Contains(err.Error(), invalid) || !strings.Contains(err.Error(), "no cameras") {
		t.Errorf("Load of an empty file = %v, %v; want an error naming it and the fault", rig, err)
	}
}

func TestLoadOfInterfacesNeedsLinux(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		t.Skip("this system binds connections to interfaces")
	}
	rig, err := Load(phoneConfig)
	if err == nil || !strings.Contains(err.Error(), "rx10m4") || !strings.Contains(err.Error(), "wlan1") {
		t.Errorf("Load = %v, %v; want an error naming camera rx10m4 and interface wlan1", rig, err)
	}
}
