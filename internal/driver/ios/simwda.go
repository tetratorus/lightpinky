package ios

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// WebDriverAgent for iOS Simulators.
//
// Pinky ships no WebDriverAgent. For a Simulator it builds one itself, with
// the Mac's own Xcode, from the source of one release of Appium's
// WebDriverAgent (github.com/appium/WebDriverAgent, Apache 2.0): the
// archive GitHub makes of the release's commit, whose files must hash to
// wdaSourceTree before anything is built from them. A Simulator needs no
// signing, so the build needs no Apple ID. The build is kept per Xcode
// build and CPU, in Dir/builds, and made again when Xcode changes.
//
// Each Simulator Pinky uses gets a test run of that build of its own
// (xcodebuild test-without-building), the way Xcode's Test starts it, with
// WDA and its MJPEG server on free ports of the Mac's loopback: a
// Simulator shares the Mac's network, and WDA would else listen on every
// interface. Pinky stops the run with SIGTERM when it closes the
// Simulator; SIGINT would make xcodebuild collect diagnostics for minutes.

// The WebDriverAgent release Pinky builds for Simulators.
const (
	wdaVersion = "16.15.3"
	wdaCommit  = "02cb92f0e93c8b021443bcbb8eeb80932c113220"
	// wdaSourceTree is the SHA-256 of the archive's files (treeHash), not
	// of the archive, whose compression GitHub may change.
	wdaSourceTree = "05f62a92a9136fcf7a0e00538d5acd47d4bdd43f375e14260fc2c718eeb2e3cf"
	// wdaSourceMax bounds the download and what it unpacks to.
	wdaSourceMax = 16 << 20
)

// wdaSourceURL is where the source comes from; a var for tests.
var wdaSourceURL = "https://github.com/appium/WebDriverAgent/archive/" + wdaCommit + ".tar.gz"

// errSourceMismatch is a download whose files are not the release's.
var errSourceMismatch = errors.New("the downloaded WebDriverAgent source is not the release lightpinky expects")

// wdaBuildTimeout bounds building WebDriverAgent; it takes 20 s to a few
// minutes, the first time Xcode compiles for the Simulator the longest.
const wdaBuildTimeout = 15 * time.Minute

// wdaBuildRetry is how long Pinky waits before building again after a
// failure, unless the person starts the Simulator again (Retry).
const wdaBuildRetry = 5 * time.Minute

// buildKey names a build: the WDA release, the Xcode build and the CPU.
func buildKey(x xcode, arch string) string {
	return wdaVersion + "-xcode" + x.Build + "-" + arch
}

// simBuild is the state of the WebDriverAgent build for one key.
type simBuild struct {
	key      string
	running  bool
	products string // the build's Products folder, once built
	err      error
	failedAt time.Time
}

// builtProducts returns the Products folder of a finished build of key in
// dir, or "" when there is none.
func builtProducts(dir, key string) string {
	p := filepath.Join(dir, "builds", key)
	if xctestrun(p) == "" {
		return ""
	}
	return p
}

// xctestrun returns the .xctestrun file in a Products folder, or "".
func xctestrun(products string) string {
	m, _ := filepath.Glob(filepath.Join(products, "*.xctestrun"))
	if len(m) == 0 {
		return ""
	}
	sort.Strings(m)
	return m[0]
}

// buildWDA downloads the source when Dir has none that checks out, builds
// it for the Simulator, and moves the build to Dir/builds/<key>, removing
// builds for other keys. It returns the Products folder.
func buildWDA(ctx context.Context, dir string, x xcode, arch string, client *http.Client) (string, error) {
	key := buildKey(x, arch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Leftovers of a build that did not finish (Pinky quit meanwhile).
	if old, _ := filepath.Glob(filepath.Join(dir, ".build-*")); len(old) > 0 {
		for _, o := range old {
			os.RemoveAll(o)
		}
	}
	tmp, err := os.MkdirTemp(dir, ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	archive := filepath.Join(dir, "WebDriverAgent-"+wdaVersion+".tar.gz")
	err = unpackSource(archive, src)
	if err != nil {
		// None yet, or not the release's: download it again.
		os.RemoveAll(src)
		if err = downloadSource(ctx, client, archive); err != nil {
			return "", err
		}
		if err = unpackSource(archive, src); err != nil {
			os.Remove(archive)
			return "", err
		}
	}
	dd := filepath.Join(tmp, "dd")
	logPath := filepath.Join(dir, "build.log")
	out, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	tail := &lastLines{max: 64 << 10}
	cmd := exec.CommandContext(ctx, x.tool("xcodebuild"), "build-for-testing",
		"-project", filepath.Join(src, "WebDriverAgent.xcodeproj"),
		"-scheme", "WebDriverAgentRunner",
		"-destination", "generic/platform=iOS Simulator",
		"-derivedDataPath", dd,
		"CODE_SIGNING_ALLOWED=NO", "ARCHS="+arch,
		// WDA builds with -Weverything -Werror, and Xcode 26 adds
		// -I /usr/local/include, which clang then refuses as unsafe.
		"OTHER_CFLAGS=$(inherited) -Wno-poison-system-directories")
	cmd.Env = x.env()
	cmd.Stdout = io.MultiWriter(out, tail)
	cmd.Stderr = cmd.Stdout
	ownGroup(cmd)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	out.Close()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("building WebDriverAgent with Xcode took too long: %w", ctx.Err())
		}
		return "", fmt.Errorf("building WebDriverAgent with Xcode %s failed: %s (%s)", x.Version, xcodeErrors(tail.String()), logPath)
	}
	products := filepath.Join(dd, "Build", "Products")
	if xctestrun(products) == "" {
		return "", fmt.Errorf("Xcode built WebDriverAgent but made no .xctestrun file (%s)", logPath)
	}
	builds := filepath.Join(dir, "builds")
	if err := os.MkdirAll(builds, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(builds, key)
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(products, final); err != nil {
		return "", err
	}
	// Builds for another Xcode or release are no use any more.
	if es, err := os.ReadDir(builds); err == nil {
		for _, e := range es {
			if e.Name() != key {
				os.RemoveAll(filepath.Join(builds, e.Name()))
			}
		}
	}
	return final, nil
}

// xcodeErrors picks what Xcode said went wrong from its output: the lines
// with "error:", else the last lines.
func xcodeErrors(out string) string {
	var errs []string
	seen := map[string]bool{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if i := strings.Index(l, "error:"); i >= 0 && !seen[l] {
			seen[l] = true
			errs = append(errs, l)
		}
	}
	if len(errs) == 0 {
		start := max(0, len(lines)-3)
		for _, l := range lines[start:] {
			if l = strings.TrimSpace(l); l != "" {
				errs = append(errs, l)
			}
		}
	}
	if len(errs) > 3 {
		errs = errs[:3]
	}
	s := strings.Join(errs, "; ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	if s == "" {
		s = "no output"
	}
	return s
}

// downloadSource fetches the release's source archive to path.
func downloadSource(ctx context.Context, client *http.Client, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wdaSourceURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "lightpinky")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading the WebDriverAgent source: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the WebDriverAgent source: GitHub answered %s", res.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".source-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // gone after the rename
	n, err := io.Copy(tmp, io.LimitReader(res.Body, wdaSourceMax+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading the WebDriverAgent source: %w", err)
	}
	if n > wdaSourceMax {
		return fmt.Errorf("downloading the WebDriverAgent source: %w", errSourceMismatch)
	}
	return os.Rename(tmp.Name(), path)
}

// unpackSource unpacks the source archive into dir, without its top
// folder, and checks that its files are the release's (wdaSourceTree).
func unpackSource(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	sum, err := untar(f, dir, wdaSourceMax)
	if err != nil {
		return err
	}
	if sum != wdaSourceTree {
		return errSourceMismatch
	}
	return nil
}

// untar unpacks a gzipped tar into dir, without the archive's one top
// folder, and returns treeHash of its files. It takes regular files and
// folders only (no links, which could point out of dir), refuses names
// that would leave dir, and stops past limit bytes.
func untar(r io.Reader, dir string, limit int64) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errSourceMismatch, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var files []treeFile
	var total int64
	top := ""
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: %v", errSourceMismatch, err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			continue // GitHub's pax header, and anything else
		}
		name := path.Clean(h.Name)
		first, rest, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
		}
		if first != top || path.IsAbs(h.Name) || strings.HasPrefix(name, "../") || name == ".." {
			return "", fmt.Errorf("%w: unexpected path %q", errSourceMismatch, h.Name)
		}
		if rest == "" || rest == "." {
			continue // the top folder itself
		}
		for _, part := range strings.Split(rest, "/") {
			if part == ".." || part == "" {
				return "", fmt.Errorf("%w: unexpected path %q", errSourceMismatch, h.Name)
			}
		}
		target := filepath.Join(dir, filepath.FromSlash(rest))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		total += h.Size
		if h.Size < 0 || total > limit {
			return "", fmt.Errorf("%w: too large", errSourceMismatch)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		exe := h.Mode&0o111 != 0
		if exe {
			mode = 0o755
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		hs := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, hs), io.LimitReader(tr, h.Size))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		if n != h.Size {
			return "", fmt.Errorf("%w: %s is cut short", errSourceMismatch, rest)
		}
		files = append(files, treeFile{rest, exe, hex.EncodeToString(hs.Sum(nil))})
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%w: no files", errSourceMismatch)
	}
	return treeHash(files), nil
}

// treeFile is one file of a source tree: its path with slashes, whether
// it is executable, and the SHA-256 of its content.
type treeFile struct {
	path string
	exe  bool
	sum  string
}

// treeHash hashes a source tree: one line per file, by path, with its
// executable bit and content hash. It changes when a file's content, name
// or executable bit does, not when the archive is compressed another way.
func treeHash(files []treeFile) string {
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	h := sha256.New()
	for _, f := range files {
		mode := "644"
		if f.exe {
			mode = "755"
		}
		fmt.Fprintf(h, "%s %s %s\n", mode, f.sum, f.path)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// simRun is WebDriverAgent's test run on one Simulator.
type simRun struct {
	cmd         *exec.Cmd
	dir         string // the run's own derived data, gone after it stops
	wda, mjpeg  int    // its ports on 127.0.0.1
	done        chan struct{}
	tail        *lastLines
	stopOnce    sync.Once
	waitErr     error
	stopTimeout time.Duration
	guard       *runGuard // ends the run when Pinky is killed
}

// startRun starts WebDriverAgent's test on a booted Simulator, from the
// build in products, with its servers on free ports of the loopback. It
// returns once xcodebuild runs; WDA answers some seconds later.
func startRun(x xcode, products, udid, runs string) (*simRun, error) {
	tr := xctestrun(products)
	if tr == "" {
		return nil, errBuildGone
	}
	wda, err := freePort()
	if err != nil {
		return nil, err
	}
	mjpeg, err := freePort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runs, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(runs, udid+"-")
	if err != nil {
		return nil, err
	}
	r := &simRun{dir: dir, wda: wda, mjpeg: mjpeg, done: make(chan struct{}), tail: &lastLines{max: 32 << 10}, stopTimeout: 5 * time.Second}
	// Not CommandContext: the run outlives the open that starts it.
	r.cmd = exec.Command(x.tool("xcodebuild"), "test-without-building",
		"-xctestrun", tr,
		"-destination", "id="+udid,
		"-derivedDataPath", dir,
		"-collect-test-diagnostics", "never",
		"-test-timeouts-enabled", "NO")
	r.cmd.Env = append(x.env(),
		// xcodebuild passes TEST_RUNNER_ variables to the test without the
		// prefix; WDA reads these.
		"TEST_RUNNER_USE_IP=127.0.0.1",
		fmt.Sprintf("TEST_RUNNER_USE_PORT=%d", wda),
		fmt.Sprintf("TEST_RUNNER_MJPEG_SERVER_PORT=%d", mjpeg))
	r.cmd.Stdout = r.tail
	r.cmd.Stderr = r.tail
	// Its own process group, so a Ctrl-C in the terminal that runs Pinky
	// does not reach it: on SIGINT it collects diagnostics for minutes.
	ownGroup(r.cmd)
	if err := r.cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("starting xcodebuild: %w", err)
	}
	if r.guard, err = guardRun(r.cmd.Process.Pid, dir); err != nil {
		r.cmd.Process.Kill()
		r.cmd.Wait()
		os.RemoveAll(dir)
		return nil, err
	}
	go func() {
		r.waitErr = r.cmd.Wait()
		r.guard.release()
		close(r.done)
	}()
	return r, nil
}

// url is WDA's address, and mjpegURL its picture's.
func (r *simRun) url() string      { return fmt.Sprintf("http://127.0.0.1:%d", r.wda) }
func (r *simRun) mjpegURL() string { return fmt.Sprintf("http://127.0.0.1:%d", r.mjpeg) }

// exited reports whether the run ended, and why.
func (r *simRun) exited() (bool, error) {
	select {
	case <-r.done:
		return true, fmt.Errorf("xcodebuild ended: %s", xcodeErrors(r.tail.String()))
	default:
		return false, nil
	}
}

// stop ends the run: SIGTERM, which xcodebuild answers by stopping the
// test within seconds, then SIGKILL. It removes the run's folder.
func (r *simRun) stop() {
	r.stopOnce.Do(func() {
		select {
		case <-r.done:
		default:
			r.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-r.done:
			case <-time.After(r.stopTimeout):
				r.cmd.Process.Kill()
				<-r.done
			}
		}
		os.RemoveAll(r.dir)
	})
}

// freePort returns a port nothing listens on at 127.0.0.1 now.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// lastLines keeps the last bytes written to it, up to max.
type lastLines struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (l *lastLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	if over := len(l.b) - l.max; over > 0 {
		l.b = l.b[over:]
		if i := bytes.IndexByte(l.b, '\n'); i >= 0 {
			l.b = l.b[i+1:]
		}
	}
	return len(p), nil
}

func (l *lastLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}
