package tester

import "os/exec"

func newCmd(bin, cfgFile string) *exec.Cmd {
	return exec.Command(bin, "run", "-config", cfgFile)
}
