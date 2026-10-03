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
// `su 2000 -c CMD`, a cmd that records each notification posted, an am that
// records each activity started, and a rig.sh that prints two lines or, with
// RIG_SIGNAL set, ends on SIGTERM. Its start first reports multicam running
// (unless RIG_NO_MULTICAM is set) and the console, then waits for an am
// start and says whether one came.
var notifyFakes = map[string]string{
	"bin/su":  "#!/bin/sh\n[ \"$1\" = 2000 ] && [ \"$2\" = -c ] || exit 9\nexec sh -c \"$3\"\n",
	"bin/cmd": "#!/bin/sh\n[ \"$1 $2 $3 $4 $5 $7\" = \"notification post -S bigtext -t rig\" ] || exit 9\nprintf '%s|%s\\036' \"$6\" \"$8\" >>\"$NOTIFY_POSTS\"\n",
	"bin/am":  "#!/bin/sh\necho \"$*\" >>\"$NOTIFY_AM\"\n",
	"mc/rig.sh": `[ -n "$RIG_SIGNAL" ] && kill -TERM $$
if [ "$1" = start ]; then
	[ -z "$RIG_NO_MULTICAM" ] && echo "multicam: running (42)"
	echo "console: http://localhost:8080/"
	i=0
	while [ ! -s "$NOTIFY_AM" ] && [ $i -lt 50 ]; do sleep 0.02; i=$((i + 1)); done
	[ -s "$NOTIFY_AM" ] && echo "console opened" || echo "console not opened"
fi
echo "rig $1"
echo line two
`,
}

// notify runs notify.sh with action beside the fake rig.sh and returns the
// notifications posted, each as "title|text", and the am commands run.
func notify(t *testing.T, action string, env ...string) (posts, am []string) {
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
	postsFile, amFile := filepath.Join(dir, "posts"), filepath.Join(dir, "am")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "mc/notify.sh"), action)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
		"NOTIFY_POSTS="+postsFile, "NOTIFY_AM="+amFile)
	cmd.Env = append(cmd.Env, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("notify.sh %s: %v (output %q)", action, err, out)
	}
	b, _ := os.ReadFile(postsFile)
	posts = strings.Split(strings.TrimSuffix(string(b), "\036"), "\036")
	b, _ = os.ReadFile(amFile)
	if len(b) > 0 {
		am = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	return posts, am
}

const openConsole = "start -a android.intent.action.VIEW -d http://localhost:8080/ -e com.android.browser.application_id multicam com.android.chrome"

func TestNotifyPostsRigOutput(t *testing.T) {
	cases := map[string][]string{
		"status": {"Rig status|rig status\nline two"},
		"stop":   {"Stop rig|rig stop\nline two"},
		"start": {"Start rig|Starting rig...",
			"Start rig|multicam: running (42)\nconsole: http://localhost:8080/\nconsole opened\nrig start\nline two"},
	}
	for action, want := range cases {
		if got, _ := notify(t, action); !slices.Equal(got, want) {
			t.Errorf("notify.sh %s posted %q; want %q", action, got, want)
		}
	}
}

func TestNotifyPostsNothingForARigEndedBySignal(t *testing.T) {
	want := []string{"Start rig|Starting rig..."}
	if got, _ := notify(t, "start", "RIG_SIGNAL=1"); !slices.Equal(got, want) {
		t.Errorf("posted %q; want %q", got, want)
	}
}

func TestNotifyStartOpensTheConsoleOnceMulticamRuns(t *testing.T) {
	_, am := notify(t, "start")
	if want := []string{openConsole}; !slices.Equal(am, want) {
		t.Errorf("am ran %q; want %q", am, want)
	}
}

func TestNotifyOpensNoConsoleWithoutMulticam(t *testing.T) {
	if _, am := notify(t, "start", "RIG_NO_MULTICAM=1"); len(am) != 0 {
		t.Errorf("am ran %q; want nothing", am)
	}
	for _, action := range []string{"stop", "status"} {
		if _, am := notify(t, action); len(am) != 0 {
			t.Errorf("notify.sh %s ran am %q; want nothing", action, am)
		}
	}
}
