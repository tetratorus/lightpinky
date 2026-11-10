package ios

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// simArch is the CPU Simulators run on: arm64 on Apple silicon, even for a
// Pinky that runs under Rosetta.
func simArch() string {
	if v, err := unix.SysctlUint32("hw.optional.arm64"); err == nil && v == 1 {
		return "arm64"
	}
	return "x86_64"
}

// ownGroup starts cmd in a process group of its own, so signals sent to
// Pinky's group, such as a terminal's Ctrl-C, do not reach it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// guardScript is the run guard's shell: $1 is the run's process group, $2
// its folder. A line on stdin is goodbye; an end of file with none means
// Pinky died. It then stops the group as simRun.stop does, SIGTERM and
// SIGKILL 5 s later, and removes the folder.
const guardScript = `read -r _ && exit 0
kill -s TERM -- -"$1" 2>/dev/null
i=0
while [ $i -lt 10 ] && kill -s 0 -- -"$1" 2>/dev/null; do sleep 0.5; i=$((i+1)); done
kill -s KILL -- -"$1" 2>/dev/null
rm -rf -- "$2"`

// runGuard ends a run that Pinky did not stop because it was killed.
// macOS has no parent-death signal, and xcodebuild leads a process group
// of its own (ownGroup), so nothing else would: it would test on, holding
// the Simulator and its ports, until someone kills it.
type runGuard struct {
	w   *os.File // the pipe only Pinky holds open
	cmd *exec.Cmd
}

// guardRun starts the guard for the run whose process group is pgid. The
// guard is a shell in a group of its own, so whatever stops Pinky's group
// does not stop it too.
func guardRun(pgid int, dir string) (*runGuard, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cmd := exec.Command("/bin/sh", "-c", guardScript, "pinky-run-guard", strconv.Itoa(pgid), dir)
	cmd.Stdin = r
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, fmt.Errorf("starting the run's guard: %w", err)
	}
	go cmd.Wait()
	return &runGuard{w: w, cmd: cmd}, nil
}

// release tells the guard the run ended, so it ends too.
func (g *runGuard) release() {
	if g == nil {
		return
	}
	g.w.Write([]byte("ended\n"))
	g.w.Close()
}
