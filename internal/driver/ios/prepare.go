package ios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/amfi"
)

// Preparing a phone for Pinky: the steps Xcode used to take. Pinky shows
// the Developer Mode switch in Settings while Developer Mode is off, mounts
// the developer disk image when the developer services are missing (below
// iOS 27 after every restart, which drops the image; on iOS 27, which keeps
// it, when another tool took it off), and on Windows asks the phone to
// trust this computer. Mounting runs in the
// background, so List says where a phone stands without waiting for it.

// devModeOff is a phone with Developer Mode off: Pinky shows the switch in
// Settings and says how to turn it on.
func (p *Provider) devModeOff(e goios.DeviceEntry, info driver.Info) driver.Info {
	info.State = driver.StateNeedsSetup
	info.Setup = driver.SetupDeveloperMode
	info.Detail = devModeDetail
	if e.Properties.ConnectionType == "Network" {
		// AMFI takes the reveal only over a cable: over Wi-Fi it closes the
		// connection without an answer (iPadOS 27.0.1, 2026-11-08), where
		// the same request over USB succeeds.
		info.Detail = devModeWiFiDetail
		return info
	}
	p.prep.revealDevMode(e, p.logger())
	return info
}

// diskImageMissing is a phone without the developer services check looks
// for: Pinky mounts the developer disk image in the background and says how
// that goes. When a mount ends, List looks at the phone again at once.
func (p *Provider) diskImageMissing(e goios.DeviceEntry, info driver.Info, check func(context.Context, goios.DeviceEntry) (bool, error)) driver.Info {
	id := e.Properties.SerialNumber
	j := p.prep.mountState(e, p.DiskImageDir, check, func() { p.forgetPhone(id) }, p.logger())
	return diskImageState(info, j, p.DiskImageDir)
}

// forgetPhone drops what List knows about a phone and its services, so the
// next List inspects it again.
func (p *Provider) forgetPhone(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.known, id)
	for k := range p.ddi {
		if k == id || strings.HasPrefix(k, id+"|") {
			delete(p.ddi, k)
		}
	}
}

// recheckFor is how long List keeps what it found about a phone that is not
// online before asking it again: briefly while the person is doing a step
// on the phone (tapping Trust, unlocking) or Pinky is preparing it, so the
// next step shows soon after.
func recheckFor(info driver.Info) time.Duration {
	switch {
	case info.State == driver.StateUnauthorized:
		return 2 * time.Second
	case info.Setup == driver.SetupPreparing, info.Setup == driver.SetupUnlock:
		return 3 * time.Second
	}
	return recheckOffline
}

// preparer holds the provider's preparation jobs. Its zero value is ready.
type preparer struct {
	mu       sync.Mutex
	jobs     map[string]*mountJob // by UDID
	revealed map[string]entryKey  // the attach the Developer Mode switch was shown for
	asked    map[string]time.Time // the last pairing request (Windows), by UDID
	// A phone whose accepted pairing did not reach the record here: the host
	// id it was stuck with, and the attach it was seen through. Pairing again
	// would only make the phone ask to be trusted while that record stays. It
	// leaves the state when the record's host id changes or the phone is
	// plugged in again.
	stuck map[string]recordStuck
	// Images Apple's signing server refused, by refusalKey, kept in
	// refusedFile in Pinky's folder for images; loaded from refusedIn.
	refused   map[string]time.Time
	refusedIn *string

	// For tests.
	findFn   func(ownDir string) (diskImage, error)
	mountFn  func(ctx context.Context, e goios.DeviceEntry, img diskImage) (mountResult, error)
	revealFn func(e goios.DeviceEntry) error
	pairFn   func(e goios.DeviceEntry) error
	recordFn func(udid string) (goios.PairRecord, error)
	now      func() time.Time
}

// mountJob is one phone's mount, running or done.
type mountJob struct {
	key     entryKey
	running bool
	ended   time.Time
	img     diskImage
	res     mountResult
	err     error
}

// errServicesMissing means the image mounted but the phone still does not
// offer the services Pinky checks for.
var errServicesMissing = errors.New("the developer disk image is mounted, but the iPhone does not offer the developer services Pinky needs")

func (pr *preparer) clock() time.Time {
	if pr.now != nil {
		return pr.now()
	}
	return time.Now()
}

// mountState returns the phone's mount job, starting one when none ran for
// this attach or when the last one failed long enough ago. When Apple's
// signing server refused the image the last one used, it starts one only
// once this computer has an image Apple has not refused, or none: the same
// image would be refused again. check tells whether the services Pinky
// needs are there; done runs when a job ends; log gets a line when a job
// starts and when it ends.
func (pr *preparer) mountState(e goios.DeviceEntry, ownDir string, check func(context.Context, goios.DeviceEntry) (bool, error), done func(), log *slog.Logger) mountJob {
	id, key := e.Properties.SerialNumber, keyOf(e)
	// Looking for the image reads files, so not while holding the lock.
	another := false
	if pr.lastRefused(id, key) {
		img, err := pr.find(ownDir)
		another = err != nil || !pr.isRefused(ownDir, img)
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.jobs == nil {
		pr.jobs = map[string]*mountJob{}
	}
	j := pr.jobs[id]
	if j != nil && j.key == key && (j.running || pr.clock().Sub(j.ended) < retryAfter(j.err) || imageRefused(j.err) && !another) {
		return *j
	}
	j = &mountJob{key: key, running: true}
	pr.jobs[id] = j
	go pr.run(j, e, ownDir, check, done, logTo(log))
	return *j
}

func (pr *preparer) run(j *mountJob, e goios.DeviceEntry, ownDir string, check func(context.Context, goios.DeviceEntry) (bool, error), done func(), log *slog.Logger) {
	mount := pr.mountFn
	if mount == nil {
		mount = mountOn
	}
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout+time.Minute)
	defer cancel()
	id, start := e.Properties.SerialNumber, time.Now()
	img, err := pr.find(ownDir)
	var res mountResult
	for err == nil {
		log.Info("iphone: mounting the developer disk image", "device", id, "image", imageName(img), "dir", img.Dir)
		res, err = mount(ctx, e, img)
		if !imageRefused(err) {
			break
		}
		// Apple will not sign it: the next image here it has not refused,
		// if any, in the same job (an official copy gives way to the
		// mirror's download).
		pr.setRefused(ownDir, img, true, log)
		next, ferr := pr.find(ownDir)
		if ferr != nil || pr.isRefused(ownDir, next) {
			break
		}
		log.Warn("iphone: Apple's signing server refused the developer disk image; trying another", "device", id, "image", imageName(img), "version", img.Version, "err", err, "next", imageName(next))
		img, err = next, nil
	}
	if err == nil {
		if !res.Already { // Apple signed it this time
			pr.setRefused(ownDir, img, false, log)
		}
		err = waitForServices(ctx, e, check)
	}
	took := time.Since(start).Round(time.Millisecond)
	switch {
	case err == nil:
		log.Info("iphone: developer disk image ready", "device", id, "way", res.How, "kept", res.Already, "replaced", res.Replaced, "took", took)
	case img.Dir == "":
		log.Warn("iphone: no developer disk image to mount", "device", id, "err", err, "retry_in", retryAfter(err))
	case imageRefused(err):
		log.Warn("iphone: Apple's signing server refused the developer disk image", "device", id, "image", imageName(img), "version", img.Version, "dir", img.Dir, "way", res.How, "took", took, "err", err, "retry", "once another image is on this computer")
	default:
		log.Warn("iphone: could not mount the developer disk image", "device", id, "way", res.How, "took", took, "err", err, "retry_in", retryAfter(err))
	}
	pr.mu.Lock()
	j.running, j.ended, j.img, j.res, j.err = false, pr.clock(), img, res, err
	pr.mu.Unlock()
	if done != nil {
		done()
	}
}

// mountOn mounts img on the phone behind e through a tunnel of its own,
// once the phone is unlocked. A locked phone fails at once with
// errPhoneLocked, so the person is asked to unlock it; so does a phone that
// locks while its image is being installed, which lockWatch notices
// within a couple of seconds, rather than after the mount's timeout.
func mountOn(ctx context.Context, e goios.DeviceEntry, img diskImage) (mountResult, error) {
	return mountOnWith(ctx, e, img, phoneStateOf, func(ctx context.Context, img diskImage, major int) (mountResult, error) {
		// The image is 16 MB sent to the phone: see uploadMTU.
		l, err := dialLinkEntryMTU(ctx, e, uploadMTU)
		if err != nil {
			return mountResult{}, err
		}
		defer l.Close()
		return mountDiskImage(ctx, l, img, major)
	})
}

// lockWatch is how often mountOnWith looks at the lock while it mounts.
var lockWatch = 2 * time.Second

// mountOnWith is mountOn with the lockdown reader and the mount to run.
func mountOnWith(ctx context.Context, e goios.DeviceEntry, img diskImage,
	state func(context.Context, goios.DeviceEntry) (phoneState, error),
	mount func(context.Context, diskImage, int) (mountResult, error)) (mountResult, error) {
	st, err := state(ctx, e)
	if err != nil {
		return mountResult{}, err
	}
	if st.locked {
		return mountResult{}, errPhoneLocked
	}
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var locked atomic.Bool
	every := lockWatch
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-mctx.Done():
				return
			case <-t.C:
				if now, err := state(mctx, e); err == nil && now.locked {
					locked.Store(true)
					cancel() // closes the mount's tunnel, which ends go-ios's wait
					return
				}
			}
		}
	}()
	r, err := mount(mctx, img, st.major)
	if err != nil && locked.Load() {
		return r, fmt.Errorf("%w (it locked while the image was being installed)", errPhoneLocked)
	}
	return r, err
}

// serviceWait is how long waitForServices waits.
var serviceWait = 10 * time.Second

// waitForServices waits up to serviceWait for the phone to publish what
// check looks for: the services come up a moment after the image mounts.
func waitForServices(ctx context.Context, e goios.DeviceEntry, check func(context.Context, goios.DeviceEntry) (bool, error)) error {
	if check == nil {
		return nil
	}
	deadline := time.Now().Add(serviceWait)
	for {
		ok, err := check(ctx, e)
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if err != nil {
				return fmt.Errorf("%w: %v", errServicesMissing, err)
			}
			return errServicesMissing
		}
		select {
		case <-ctx.Done():
		case <-time.After(serviceWait / 10):
		}
	}
}

// retryAfter is how long a mount waits before the next try, by what went
// wrong: soon for what the person fixes in a moment (unlocking), later for
// what needs more (the network, installing Xcode), and 10 minutes or until
// the phone reconnects for what trying again cannot fix. A mount that
// worked is tried again only if the services go missing again (another tool
// took the image off).
func retryAfter(err error) time.Duration {
	if err == nil {
		return 30 * time.Second
	}
	switch {
	case errors.Is(err, errServicesMissing):
		return 10 * time.Minute
	}
	switch classifyMountError(err) {
	case problemLocked, problemDevMode:
		return 3 * time.Second
	case problemNoImage:
		return 10 * time.Second
	case problemNetwork:
		return 20 * time.Second
	case problemNewer, problemNotFor:
		return 10 * time.Minute
	case problemRefused:
		// Not with the same image (mountState), at once with another.
		return 0
	}
	return 30 * time.Second
}

// imageRefused reports whether a mount failed because Apple's signing
// server would not sign the image.
func imageRefused(err error) bool {
	return err != nil && classifyMountError(err) == problemRefused
}

// lastRefused reports whether the phone's last mount on this attach ended
// with Apple's signing server refusing the image.
func (pr *preparer) lastRefused(id string, key entryKey) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	j := pr.jobs[id]
	return j != nil && j.key == key && !j.running && imageRefused(j.err)
}

// find looks for the developer disk image to mount, passing over those
// Apple's signing server refused while there is another.
func (pr *preparer) find(ownDir string) (diskImage, error) {
	if pr.findFn != nil {
		return pr.findFn(ownDir)
	}
	return findDiskImageAvoiding(ownDir, func(img diskImage) bool { return pr.isRefused(ownDir, img) })
}

// refusedFile, in Pinky's folder for images, keeps the images Apple's
// signing server refused across runs, so Pinky does not go back to one:
// {"<build> <version>": "<when>"}.
const refusedFile = "refused.json"

// refusalKey names an image in the refusals: by build and version, which
// stand for its digests, which are what Apple decides by.
func refusalKey(img diskImage) string {
	return img.Build + " " + img.Version
}

// isRefused reports whether Apple's signing server refused img.
func (pr *preparer) isRefused(ownDir string, img diskImage) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.loadRefusedLocked(ownDir)
	_, ok := pr.refused[refusalKey(img)]
	return ok
}

// setRefused records that Apple's signing server refused img, or signed it
// (refused false), in memory and in refusedFile when it changed.
func (pr *preparer) setRefused(ownDir string, img diskImage, refused bool, log *slog.Logger) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.loadRefusedLocked(ownDir)
	k := refusalKey(img)
	if _, had := pr.refused[k]; had == refused {
		return
	}
	if refused {
		pr.refused[k] = pr.clock()
	} else {
		delete(pr.refused, k)
	}
	if ownDir == "" {
		return
	}
	b, err := json.MarshalIndent(pr.refused, "", "  ")
	if err == nil {
		err = os.MkdirAll(ownDir, 0o755)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(ownDir, refusedFile), b, 0o644)
	}
	if err != nil {
		logTo(log).Warn("iphone: could not keep the developer disk images Apple refused", "err", err)
	}
}

// loadRefusedLocked reads refusedFile from ownDir once, with pr.mu held.
func (pr *preparer) loadRefusedLocked(ownDir string) {
	if pr.refusedIn != nil && *pr.refusedIn == ownDir {
		return
	}
	pr.refusedIn = &ownDir
	pr.refused = readRefusals(ownDir)
}

// readRefusals reads refusedFile from ownDir: none when there is none,
// and a damaged file only forgets.
func readRefusals(ownDir string) map[string]time.Time {
	refused := map[string]time.Time{}
	if ownDir == "" {
		return refused
	}
	if b, err := os.ReadFile(filepath.Join(ownDir, refusedFile)); err == nil {
		_ = json.Unmarshal(b, &refused)
		if refused == nil {
			refused = map[string]time.Time{}
		}
	}
	return refused
}

// forget drops a phone's job, so the next mountState starts a new one.
func (pr *preparer) forget(id string) {
	pr.mu.Lock()
	delete(pr.jobs, id)
	pr.mu.Unlock()
}

// revealDevMode shows the Developer Mode switch in the phone's Settings ›
// Privacy & Security, as Xcode does: iOS hides it until a developer tool
// has seen the phone. Once per attach, in the background.
func (pr *preparer) revealDevMode(e goios.DeviceEntry, log *slog.Logger) {
	log = logTo(log)
	id, key := e.Properties.SerialNumber, keyOf(e)
	pr.mu.Lock()
	if pr.revealed == nil {
		pr.revealed = map[string]entryKey{}
	}
	if k, ok := pr.revealed[id]; ok && k == key {
		pr.mu.Unlock()
		return
	}
	pr.revealed[id] = key
	reveal := pr.revealFn
	pr.mu.Unlock()
	if reveal == nil {
		reveal = revealDevMode
	}
	go func() {
		// The person's step is the same either way; a failed reveal is
		// sent again at the next look.
		if err := reveal(e); err != nil {
			log.Warn("iphone: could not show the Developer Mode switch", "device", id, "err", err)
			pr.mu.Lock()
			if pr.revealed[id] == key {
				delete(pr.revealed, id)
			}
			pr.mu.Unlock()
			return
		}
		log.Info("iphone: showed the Developer Mode switch in Settings", "device", id)
	}()
}

func revealDevMode(e goios.DeviceEntry) error {
	c, err := amfi.New(e)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.RevealDevMode()
}

// askTrust makes a phone that has never trusted this computer show "Trust
// This Computer?", on Windows only: macOS and Linux's usbmuxd ask by
// themselves when a phone is plugged in, and a second pairing there could
// race theirs. At most every 4 s while the phone waits; a pairing that the
// person accepted saves its record through usbmuxd, as Apple's tools do.
//
// lockErr is why lockdown refused the phone. A phone whose answer shows that
// it does not take the record this computer sends it (staleRecord) is asked
// to trust this computer again: tapping Trust heals nothing while the record
// stays, and a pairing is what replaces it. Once a pairing comes back with
// the record unchanged, askTrust stops pairing: pairing makes a new host id
// every time and may ask the person to trust the computer again, while the
// step says the record here needs an administrator. It starts pairing again
// when the record's host id changes, or when the phone is plugged in again.
// askTrust returns the error that still explains the refusal: lockErr when it
// paired nothing, the pairing's own error when it failed, and a
// recordStuckError while the record here cannot take a pairing.
func (pr *preparer) askTrust(e goios.DeviceEntry, lockErr error, log *slog.Logger) error {
	pair, record := pr.pairFn, pr.recordFn
	if pair == nil {
		if runtime.GOOS != "windows" {
			return lockErr
		}
		pair, record = goios.Pair, goios.ReadPairRecord
	}
	id, attach := e.Properties.SerialNumber, keyOf(e)
	pr.mu.Lock()
	if pr.stuck == nil {
		pr.stuck = map[string]recordStuck{}
	}
	stuck, wasStuck := pr.stuck[id]
	if wasStuck && stuck.attach != attach {
		// A new usbmuxd attach: the person may have repaired the record
		// and plugged the phone in again.
		delete(pr.stuck, id)
		wasStuck = false
	}
	pr.mu.Unlock()
	if wasStuck {
		// A changed host id means Apple's device service replaced the
		// record it could not write before. While it stays, say why and
		// ask the phone to pair no more.
		if hostID(record, id) != stuck.hostID {
			pr.setStuck(id, attach, "", false)
		} else {
			return recordStuckError{udid: id}
		}
	}
	if record != nil && !staleRecord(lockErr) {
		if _, err := record(id); err == nil {
			pr.setStuck(id, attach, "", false)
			return lockErr // paired, and the phone takes the record; lockdown says why it still refuses
		}
	}
	pr.mu.Lock()
	if pr.asked == nil {
		pr.asked = map[string]time.Time{}
	}
	t, asked := pr.asked[id]
	if asked && pr.clock().Sub(t) < 4*time.Second {
		pr.mu.Unlock()
		return lockErr
	}
	pr.asked[id] = pr.clock()
	pr.mu.Unlock()
	if !asked {
		logTo(log).Info("iphone: asking the iPhone to trust this computer", "device", id)
	}
	before := hostID(record, id)
	if err := pair(e); err != nil {
		return err
	}
	logTo(log).Info("iphone: the iPhone trusts this computer", "device", id)
	if before != "" && hostID(record, id) == before {
		// A pairing is a new host id every time: the record this computer
		// holds is the one from before, so Apple's device service did not
		// take the pairing the phone accepted.
		logTo(log).Warn("iphone: the pairing the iPhone accepted did not reach this computer's pairing record", "device", id)
		pr.setStuck(id, attach, before, true)
		return recordStuckError{udid: id}
	}
	pr.setStuck(id, attach, "", false)
	return nil
}

// recordStuck is a phone whose accepted pairing did not reach the record
// here, with the host id the record was stuck with and the attach it was
// seen through.
type recordStuck struct {
	hostID string
	attach entryKey
}

// setStuck remembers or forgets a phone whose record cannot take a pairing.
func (pr *preparer) setStuck(id string, attach entryKey, hostID string, stuck bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if !stuck {
		delete(pr.stuck, id)
		return
	}
	if pr.stuck == nil {
		pr.stuck = map[string]recordStuck{}
	}
	pr.stuck[id] = recordStuck{hostID: hostID, attach: attach}
}

// staleRecord says lockdown refused the phone because it does not take the
// pairing record this computer sends it: the record another install of
// Apple's software wrote, which Apple's device service could not replace.
func staleRecord(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "invalidhostid") || strings.Contains(m, "invalid pair record") ||
		strings.Contains(m, "invalidpairrecord")
}

// hostID is the host id of the pairing record this computer has for udid, and
// "" when it has none or cannot read it. record may be nil, in a test.
func hostID(record func(string) (goios.PairRecord, error), udid string) string {
	if record == nil {
		return ""
	}
	r, err := record(udid)
	if err != nil {
		return ""
	}
	return r.HostID
}

// recordStuckError is a phone that trusts this computer and whose new pairing
// the pairing record here did not take. Apple's device service writes the
// record, and it cannot replace one that another install of Apple's software
// wrote, so the phone keeps refusing the record it holds however often the
// person taps Trust (recordStuckDetail says how to let the service write it).
type recordStuckError struct{ udid string }

func (e recordStuckError) Error() string {
	return "the iPhone trusts this computer, but Apple's device service could not replace its pairing record for " + e.udid
}

// recordStuckDetail says how to let Apple's device service replace the
// pairing record: on Windows the record lives in ProgramData, where only an
// administrator may give the service the right to replace it.
func recordStuckDetail(udid string) string {
	return "The iPhone trusts this computer, but Apple's device service could not replace this computer's " +
		"pairing record, which was probably written by another install of Apple's software. " +
		"Run this in PowerShell as an administrator, then unplug the iPhone and plug it in again:\n" +
		driver.PairRecordFixCommand(udid)
}

// Sentences for the steps, in driver.Info.Detail.
const (
	trustDetail       = "Unlock the iPhone and tap Trust on the \"Trust This Computer?\" prompt, then enter its passcode."
	trustDeniedDetail = "The iPhone was told not to trust this computer. Unplug it and plug it in again, then tap Trust."
	devModeDetail     = "Turn on Developer Mode: on the iPhone, open Settings > Privacy & Security > Developer Mode, turn it on and tap Restart. After the restart, unlock the iPhone, tap Turn On and enter its passcode."
	devModeWiFiDetail = devModeDetail + " If Developer Mode is not in Privacy & Security, plug the iPhone in once: Pinky can make the switch appear only over a cable."
	preparingDetail   = "Pinky is preparing the iPhone: it mounts Apple's developer disk image, which takes a few seconds."
	unlockDetail      = "Unlock the iPhone so Pinky can finish preparing it."
)

// trustState fills in a phone that does not let this computer read it yet.
func trustState(info driver.Info, err error) driver.Info {
	info.State = driver.StateUnauthorized
	info.Setup = driver.SetupTrust
	info.Detail = trustDetail
	if err == nil {
		return info
	}
	var stuck recordStuckError
	switch m := strings.ToLower(err.Error()); {
	case errors.As(err, &stuck):
		info.Setup = driver.SetupTrustRecord
		info.Detail = recordStuckDetail(stuck.udid)
	case strings.Contains(m, "userdenied"):
		info.Detail = trustDeniedDetail
	}
	return info
}

// mountState fills in a phone whose developer disk image is being mounted
// or did not mount. version is the phone's iOS version.
func diskImageState(info driver.Info, j mountJob, ownDir string) driver.Info {
	info.State = driver.StateNeedsSetup
	info.Setup = driver.SetupDiskImage
	switch {
	case j.running:
		info.Setup, info.Detail = driver.SetupPreparing, preparingDetail
	case j.err == nil:
		// Mounted, and the services came up: List looks again.
		info.Setup, info.Detail = driver.SetupPreparing, preparingDetail
	case errors.Is(j.err, errServicesMissing):
		if v := parseVersion(info.OSVersion); j.img.Version != "" && parseVersion(j.img.Version).Major < v.Major {
			info.Detail = fmt.Sprintf("The developer disk image on this computer (from Xcode %s) is too old for iOS %s. Update Xcode to %d or later and open it once.", j.img.Build, info.OSVersion, v.Major)
		} else {
			info.Detail = "The developer disk image is mounted, but the iPhone does not offer the services Pinky needs. Restart the iPhone and connect it again."
		}
	default:
		switch classifyMountError(j.err) {
		case problemLocked:
			info.Setup, info.Detail = driver.SetupUnlock, unlockDetail
		case problemDevMode:
			info.Setup, info.Detail = driver.SetupDeveloperMode, devModeDetail
		case problemNoImage:
			info.Setup, info.Detail = driver.SetupNoDiskImage, noImageDetail(ownDir)
		case problemNetwork:
			info.Detail = "Pinky could not reach Apple's signing server (gs.apple.com), which signs the developer disk image for each iPhone. Check the internet connection."
		case problemRefused:
			info.Setup, info.Detail = driver.SetupDiskImageRefused, refusedDetail(j.img, j.err)
			info.DiskImage = &driver.DiskImage{Source: j.img.Source, Build: j.img.Build, Version: j.img.Version, Dir: j.img.Dir}
		case problemNotFor:
			info.Detail = fmt.Sprintf("The developer disk image on this computer (from Xcode %s) does not support this iPhone. Update Xcode and open it once.", j.img.Build)
		case problemNewer:
			info.Detail = "The iPhone has a newer developer disk image than this computer, and its developer services are missing. Restart the iPhone and connect it again."
		default:
			info.Detail = "Pinky could not mount the developer disk image: " + lastLine(j.err)
		}
	}
	return info
}

// refusedDetail says that Apple's signing server would not sign img, why
// (err), and how to get an image it signs, by where img came from.
func refusedDetail(img diskImage, err error) string {
	which := imageName(img)
	if img.Version != "" {
		which += ", version " + img.Version
	}
	s := fmt.Sprintf("Apple's signing server would not sign the developer disk image Pinky found for this iPhone (%s, in %s): %s. Apple signs only recent images.", which, img.Dir, appleAnswer(err))
	switch img.Source {
	case sourceXcode:
		return s + " Install or update Xcode from the App Store and open it once: it installs a newer image."
	case sourceMirror:
		return s + " Install Xcode from the App Store and open it once, or update Pinky when a version with a newer image is out."
	}
	return s + " Replace it with the iOS_DDI folder of a Mac where the latest Xcode from the App Store was opened once."
}

// appleAnswer is what Apple's signing server said in err, such as
// `status 94: "This device isn't eligible for the requested build."`.
func appleAnswer(err error) string {
	s := lastLine(err)
	const lead = "unexpected status in response "
	if i := strings.Index(s, lead); i >= 0 {
		return "status " + s[i+len(lead):]
	}
	return s
}

// noImageDetail says where to get a developer disk image.
func noImageDetail(ownDir string) string {
	folder := "Pinky's folder"
	if ownDir != "" {
		folder = filepath.Join(ownDir, "iOS_DDI")
	}
	mirror := " Add phone can also download it from a third-party mirror, " + mirrorName + "."
	if runtime.GOOS == "darwin" {
		return "Pinky needs Apple's developer disk image, which comes with Xcode. Install Xcode from the App Store and open it once (it installs the image; you can quit it then), or put an iOS_DDI folder in " + folder + "." + mirror
	}
	return "Pinky needs Apple's developer disk image, which comes with Xcode. Copy /Library/Developer/DeveloperDiskImages/iOS_DDI from a Mac where Xcode was opened once to " + folder + "." + mirror
}

// lastLine shortens a joined error to its last line, the last way tried.
func lastLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// phoneState is what lockdown says before a mount: the iOS version's
// major number and whether the phone is locked (PasswordProtected, as
// Device.Locked reads it).
type phoneState struct {
	major  int
	locked bool
}

// phoneStateOf asks lockdown, in one session.
func phoneStateOf(ctx context.Context, e goios.DeviceEntry) (phoneState, error) {
	type result struct {
		st  phoneState
		err error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := goios.ConnectLockdownWithSession(e)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer conn.Close()
		var st phoneState
		v, err := conn.GetValue("ProductVersion")
		if err != nil {
			ch <- result{err: err}
			return
		}
		s, _ := v.(string)
		st.major = parseVersion(s).Major
		v, err = conn.GetValue("PasswordProtected")
		if err != nil {
			ch <- result{err: err}
			return
		}
		b, ok := v.(bool)
		if !ok {
			ch <- result{err: fmt.Errorf("PasswordProtected is %T", v)}
			return
		}
		st.locked = b
		ch <- result{st: st}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-ctx.Done():
		return phoneState{}, ctx.Err()
	}
}

// lockedEntry reports whether the phone behind e is locked.
func lockedEntry(ctx context.Context, e goios.DeviceEntry) (bool, error) {
	st, err := phoneStateOf(ctx, e)
	return st.locked, err
}

// discard takes the lines of a provider without a Log.
var discard = slog.New(slog.DiscardHandler)

func logTo(l *slog.Logger) *slog.Logger {
	if l == nil {
		return discard
	}
	return l
}
