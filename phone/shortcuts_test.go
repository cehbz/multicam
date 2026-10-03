package phone

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShortcutsRunNotifyAsRoot runs each Termux:Widget task with an su that
// prints its arguments: each task hands its action to notify.sh through su.
func TestShortcutsRunNotifyAsRoot(t *testing.T) {
	actions := map[string]string{"Start rig": "start", "Stop rig": "stop", "Rig status": "status"}
	names, err := filepath.Glob("shortcuts/tasks/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(actions) {
		t.Errorf("tasks %q; want one each of %v", names, actions)
	}
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "su"), []byte("#!/bin/sh\necho su \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		t.Run(filepath.Base(name), func(t *testing.T) {
			action, ok := actions[filepath.Base(name)]
			if !ok {
				t.Fatalf("unexpected task %s", name)
			}
			cmd := exec.Command("sh", name)
			cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("exit: %v", err)
			}
			if got, want := strings.TrimSpace(string(out)), "su -c /data/local/tmp/mc/notify.sh "+action; got != want {
				t.Errorf("ran %q; want %q", got, want)
			}
		})
	}
}
