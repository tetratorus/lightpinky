//go:build !darwin

package ios

import "os/exec"

// Simulators run on macOS only; these keep the package building elsewhere.

func simArch() string { return "x86_64" }

func ownGroup(*exec.Cmd) {}

type runGuard struct{}

func guardRun(int, string) (*runGuard, error) { return nil, nil }

func (*runGuard) release() {}
