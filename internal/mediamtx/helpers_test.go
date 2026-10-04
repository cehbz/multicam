package mediamtx

import (
	"context"
	"os/exec"
	"testing"
)

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithCancel(t.Context())
}

// startSleeper starts a process that does nothing for a minute.
func startSleeper() (*exec.Cmd, error) {
	cmd := exec.Command("sleep", "60")
	return cmd, cmd.Start()
}
