//go:build windows

package agentrunner

import "os/exec"

func configureProcessGroup(command *exec.Cmd) {}

func terminateProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
