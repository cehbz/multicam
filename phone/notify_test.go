package phone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// notifyFakes are the commands notify.sh meets: an su that runs only
// `su 2000 -c CMD`, a cmd that records each notification posted, and a
// rig.sh that prints two lines or, with RIG_SIGNAL set, ends on SIGTERM.
var notifyFakes = map[string]string{
	"bin/su":  "#!/bin/sh\n[ \"$1\" = 2000 ] && [ \"$2\" = -c ] || exit 9\nexec sh -c \"$3\"\n",
	"bin/cmd": "#!/bin/sh\n[ \"$1 $2 $3 $4 $5 $7\" = \"notification post -S bigtext -t rig\" ] || exit 9\nprintf '%s|%s\\036' \"$6\" \"$8\" >>\"$NOTIFY_POSTS\"\n",
	"mc/rig.sh": "[ -n \"$RIG_SIGNAL\" ] && kill -TERM $$\necho \"rig $1\"\necho line two\n",
}

// notify runs notify.sh with action beside the fake rig.sh and returns the
// notifications posted, each as "title|text".
func notify(t *testing.T, action string, env ...string) []string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range notifyFakes {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("notify.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mc/notify.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	posts := filepath.Join(dir, "posts")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "mc/notify.sh"), action)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
		"NOTIFY_POSTS="+posts)
	cmd.Env = append(cmd.Env, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("notify.sh %s: %v (output %q)", action, err, out)
	}
	b, _ := os.ReadFile(posts)
	return strings.Split(strings.TrimSuffix(string(b), "\036"), "\036")
}

func TestNotifyPostsRigOutput(t *testing.T) {
	cases := map[string][]string{
		"status": {"Rig status|rig status\nline two"},
		"stop":   {"Stop rig|rig stop\nline two"},
		"start":  {"Start rig|Starting rig...", "Start rig|rig start\nline two"},
	}
	for action, want := range cases {
		if got := notify(t, action); !slices.Equal(got, want) {
			t.Errorf("notify.sh %s posted %q; want %q", action, got, want)
		}
	}
}

func TestNotifyPostsNothingForARigEndedBySignal(t *testing.T) {
	want := []string{"Start rig|Starting rig..."}
	if got := notify(t, "start", "RIG_SIGNAL=1"); !slices.Equal(got, want) {
		t.Errorf("posted %q; want %q", got, want)
	}
}
