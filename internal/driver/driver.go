// Package driver defines what a platform driver provides: device discovery,
// observation, input and a video stream. The gateway never branches on the
// platform; it only talks to these interfaces.
package driver

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Platform is a phone platform.
type Platform string

// Platforms.
const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

// State is a device's connection state.
type State string

// Device states.
const (
	StateOnline       State = "online"
	StateOffline      State = "offline"
	StateUnauthorized State = "unauthorized" // the phone has not trusted this computer yet
	StateNeedsSetup   State = "needs_setup"  // e.g. Developer Mode off; Detail says what to do
)

// Info describes a device.
type Info struct {
	ID         string // adb serial or iOS UDID; stable across reconnects
	Platform   Platform
	Name       string // "Pixel 9", or the name the owner gave the iPhone
	Model      string
	OSVersion  string
	Connection string // "usb" or "wifi"
	Backend    string // "scrcpy", "coredevice", "wda", "mirroring", "fake"
	State      State
	Detail     string // a plain sentence when State is not online
	// Setup names the step a phone that is not online waits for, one of
	// the Setup constants, so a client can walk the person through it in
	// its own words; "" when the driver does not say. Detail says the same
	// in a sentence.
	Setup string
	// HelperExpires is when the app the phone is driven through stops
	// starting because the provisioning profile it was signed with runs
	// out: iPhones driven through the WebDriverAgent helper, whose profile
	// lasts 7 days with a free Apple ID and a year with a paid developer
	// account. Zero when unknown, and for phones driven another way.
	// Clients remind the person HelperReminder before; once it has passed
	// the phone needs setup, with SetupInstallHelper.
	HelperExpires time.Time `json:",omitzero"`
	// Via says how a phone connected over the network ("wifi") is reached
	// when that is not the local network: ViaTailscale through Tailscale;
	// "" on the computer's own network or by cable.
	Via string
	// DiskImage is the developer disk image a phone waiting with
	// SetupDiskImageRefused was refused; nil otherwise.
	DiskImage *DiskImage `json:",omitempty"`
	// WiFi is what the provider knows of reaching an iPhone or iPad over
	// Wi-Fi, for its checkup and diagnostics; zero for other phones.
	WiFi WiFiConnection `json:",omitzero"`
	// Virtual marks a phone that runs on this computer: an iOS Simulator
	// or an Android emulator (VirtualLauncher). Its Connection is "usb",
	// as for a phone on a cable.
	Virtual bool `json:",omitempty"`
	// VirtualID is the VirtualPhone.ID a virtual phone runs as, to start
	// it again from its page while it is offline; "" when not known.
	VirtualID string `json:",omitempty"`
}

// WiFiConnection is what an iPhone's provider knows of its Wi-Fi connection:
// whether the phone allows this computer to reach it over Wi-Fi (Finder's
// "Show this iPhone when on Wi-Fi", Xcode's "Connect via network", the
// lockdown value EnableWifiConnections), who turned that on, and whether
// this computer lists it over the network now.
type WiFiConnection struct {
	// Connections is one of the WiFiConnections values; "" when the driver
	// does not tell.
	Connections string
	// Detail says, in English, why turning them on failed or why Pinky did
	// not look.
	Detail string `json:",omitempty"`
	// Seen: this computer lists the phone over the network now.
	Seen bool `json:",omitempty"`
}

// WiFiConnection.Connections values.
const (
	WiFiConnectionsOn       = "on"        // they are on: listed over the network, or read so on a cable
	WiFiConnectionsTurnedOn = "turned_on" // Pinky turned them on when the phone was plugged in
	WiFiConnectionsFailed   = "failed"    // Pinky could not turn them on: Detail says why
	WiFiConnectionsOff      = "off"       // Pinky does not use phones over Wi-Fi (Settings › Phones), so it did not look
	WiFiConnectionsUnknown  = "unknown"   // not looked at: the phone does not trust this computer or did not answer (Detail), or was not plugged in since Pinky started
)

// DiskImage is a developer disk image on the computer (iPhones).
type DiskImage struct {
	Source  string // where it was found, one of the DiskImageFrom constants
	Build   string // the Xcode build it came with, such as "27B52"
	Version string // its version on the phone, such as "27.2.52.0"; "" for images older than Xcode 16
	Dir     string // its folder
}

// Where a developer disk image was found, in DiskImage.Source.
const (
	DiskImageFromXcode  = "xcode"  // installed by Xcode on this Mac
	DiskImageFromMirror = "mirror" // downloaded from the third-party mirror (DiskImageDownloader)
	DiskImageFromFolder = "pinky"  // put in Pinky's own folder
	DiskImageFromEnv    = "env"    // named by the PINKY_DDI environment variable
)

// Setup steps, in Info.Setup.
const (
	SetupTrust         = "trust"          // unlock the phone, tap Trust and enter its passcode
	SetupTrustRecord   = "trust_record"   // the phone trusts the computer, but Apple's device service cannot replace the pairing record it holds; Detail says how to let it
	SetupDeveloperMode = "developer_mode" // turn on Developer Mode, which restarts the phone
	SetupUnlock        = "unlock"         // unlock the phone so the computer can finish preparing it
	SetupPreparing     = "preparing"      // the computer is preparing the phone; nothing to do but wait
	SetupNoDiskImage   = "no_disk_image"  // the computer has no developer disk image; Detail says where to get one
	SetupDiskImage     = "disk_image"     // the developer disk image did not mount; Detail says why
	SetupUpdate        = "update"         // the phone's OS is too old for Pinky
	SetupPasscode      = "passcode"       // enter the passcode where the phone asks for it: iOS does now and then before the WebDriverAgent helper may control it
	SetupConnection    = "connection"     // the connection chosen for the phone (Settings › Phones) does not work with it; Detail says why and what to choose
	// SetupSimulatorHelper: an iOS Simulator that Pinky cannot drive yet,
	// as building the WebDriverAgent helper for it with Xcode failed
	// (Xcode's license not agreed to, its iOS platform not installed, the
	// helper's source not downloaded). Detail says why. Pinky tries again
	// by itself, and at once when the person starts the Simulator again.
	SetupSimulatorHelper = "simulator_helper"
	// SetupDiskImageRefused: Apple's signing server would not sign the
	// developer disk image on the computer (Info.DiskImage) for the phone,
	// as it does with images it no longer signs, such as those of Xcode
	// before 27. Trying the same image again cannot help: the computer
	// needs a newer one, which the provider tries as soon as it is there.
	SetupDiskImageRefused = "disk_image_refused"
	// A phone reached over the network (WiFiSwitch, Tailnet) that is
	// offline: its adb stopped listening, as when it restarted (plug it in
	// once and turn its Wi-Fi connection on again), or it is not on the
	// network now (Pinky connects again by itself).
	SetupNetworkOff  = "network_off"
	SetupNetworkAway = "network_away"
	// SetupConfirmWiFi: the phone is reachable only over Wi-Fi and Pinky
	// has not used it, so it may not be the person's (a family member's
	// iPad once paired with this computer): the person says whether it is
	// theirs. Until then nothing is done on the phone (WiFiFinder).
	SetupConfirmWiFi = "confirm_wifi"
	// SetupDeviceService: the computer's device service (usbmuxd on a Mac)
	// lost the phone on Wi-Fi: the phone is on the network, but the service
	// no longer lists it, and will not for up to an hour. Restarting the
	// service (DeviceServiceRestarter) brings it back.
	SetupDeviceService = "device_service"
	// SetupPhoneChanged: an Android phone reached over the network is not
	// the phone Pinky set up under its id: it lacks the identity mark
	// Pinky left on that phone (a phone that was reset, or another device
	// on the network that claims to be it). Nothing is done on it until
	// the person trusts it (PhoneTruster) or plugs the phone in.
	SetupPhoneChanged = "phone_changed"
	// SetupNotOnWiFi: an iPhone whose cable was just pulled, and whose
	// Wi-Fi connection is on, is not seen on Wi-Fi: neither the device
	// service lists it nor does the network show it (another network, one
	// that keeps devices apart, a VPN, asleep). Listed offline: it is not
	// connected, and no step on the computer brings it back.
	SetupNotOnWiFi = "not_on_wifi"
)

// Provider discovers and opens the devices of one platform.
type Provider interface {
	Platform() Platform
	List(ctx context.Context) ([]Info, error)
	Open(ctx context.Context, id string) (Device, error)
}

// HostChecker is a Provider that needs software on the computer (usbmuxd
// for iPhones) and says when it is missing, since a provider without it
// lists no phones at all.
type HostChecker interface {
	// CheckHost reports what the computer lacks; the zero HostCheck means
	// nothing.
	CheckHost() HostCheck
}

// HostCheck is what HostChecker found missing.
type HostCheck struct {
	// Missing is a code clients put into words: "apple_devices" (Windows:
	// Apple's iPhone driver, from the Apple Devices app or iTunes),
	// "usbmuxd" (Linux: the usbmuxd package), "usbmuxd_stopped" (Linux:
	// installed, but not running; it starts when an iPhone is plugged in).
	Missing string
	Detail  string // the same in a sentence
}

// OpenChecker is a Provider that can tell why a phone it lists as online
// did not open, for the phone's checkup, which otherwise says only that the
// phone is not connected.
type OpenChecker interface {
	// OpenChecks returns checkup items about the phone's last open, which
	// failed: nil when it opened, was not tried or failed for a reason the
	// provider has no item for.
	OpenChecks(deviceID string) []Check
}

// VirtualLauncher is a Provider that lists the virtual phones this computer
// has (iOS Simulators, Android emulators) and starts and stops them. Only
// the person does that, from the client: agents get no tool for it. A
// virtual phone that runs is listed by List like any phone, with
// Info.Virtual.
type VirtualLauncher interface {
	VirtualPhones(ctx context.Context) (VirtualPhones, error)
	// StartVirtual starts a virtual phone by its VirtualPhone.ID and
	// returns once it started; it may still be booting.
	StartVirtual(ctx context.Context, id string) error
	// StopVirtual shuts a running virtual phone down.
	StopVirtual(ctx context.Context, id string) error
}

// VirtualPhones is what a VirtualLauncher found.
type VirtualPhones struct {
	Platform Platform
	Phones   []VirtualPhone
	// Missing is a code clients put into words when the computer lacks
	// what the platform's virtual phones need: "xcode" (no Xcode, or its
	// command line tools selected instead), "android_sdk" (no Android SDK
	// found), "emulator" (an SDK without its emulator). "" when nothing is
	// missing; there may still be no phones, as before any is created.
	Missing string
	Detail  string // the same in a sentence
	// Where says where the tools were found: the Xcode or the Android SDK
	// folder.
	Where string
}

// VirtualPhone is one virtual phone: an iOS Simulator device, or an
// Android Virtual Device.
type VirtualPhone struct {
	ID        string // the Simulator's UDID, or the AVD's name
	Name      string // the name the person gave it
	Model     string // the device it is made as: "iPhone 17", "Pixel 9"
	OSVersion string // "26.5", "16"
	Running   bool
	// DeviceID is its phone's ID while it runs (Info.ID): the Simulator's
	// UDID, or the emulator's adb serial; "" when it is not running.
	DeviceID string
	// Starting says it is booting; DeviceID may not be listed yet.
	Starting bool
	// Failed says why it quit while it was starting, as an emulator that
	// crashed while it booted; "" otherwise, and once it runs or starts
	// again.
	Failed string `json:",omitempty"`
	// Unavailable says why it cannot start, as when its system image or
	// its runtime is missing; "" when it can.
	Unavailable string
}

// Device is an open, long-lived session with one phone. Methods must be safe
// to call from several goroutines; implementations serialize as needed.
type Device interface {
	Info() Info
	// Observe reads the current screen. Refs are assigned by the gateway.
	Observe(ctx context.Context, opt ObserveOptions) (*screen.Screen, error)
	// Screenshot returns the screen in the same pixel space as Observe.
	Screenshot(ctx context.Context) (image.Image, error)
	Tap(ctx context.Context, p screen.Point, opt TapOptions) error
	Swipe(ctx context.Context, from, to screen.Point, dur time.Duration) error
	// Type enters text into the focused field.
	Type(ctx context.Context, text string, opt TypeOptions) error
	Key(ctx context.Context, k Key) error
	// Launch starts an app by package name, bundle id or display name.
	Launch(ctx context.Context, app string) error
	Apps(ctx context.Context) ([]App, error)
	// Stream starts the live video stream. One stream per device at a time.
	Stream(ctx context.Context, opt StreamOptions) (Stream, error)
	Close() error
}

// ObserveOptions tune Observe.
type ObserveOptions struct {
	// OCR asks the driver to add OCR elements for areas the tree misses.
	// A driver may find that its tree misses nothing and read no pixels:
	// Android reads by OCR only where the tree is empty or sparse over a
	// web view or a canvas, unless OCRAll is set as well.
	OCR bool
	// OCRAll, with OCR, asks for OCR elements for all the text the tree
	// does not show, however complete the tree looks: the agent asked for
	// the screen's text, or the gateway checks the text under a finger.
	// Drivers that read every OCR read this way (iOS) ignore it.
	OCRAll bool
	// Frame, when set, returns a picture of the screen decoded from its
	// live video, which the gateway shares with the mirror. A driver may
	// read OCR's text from it where its own screenshots are slow (an
	// iPhone over Wi-Fi: about 5 s a screenshot; an Android background
	// display, which has none); nil means none.
	Frame func(ctx context.Context) (image.Image, error)
	// MayCapture, when set, reports whether the driver may capture the
	// screen it read, as a screenshot or a frame of the video kept in
	// memory only, to read its text by OCR: not of a locked phone, nor of
	// a blocked app unless the person allowed it for the task. The
	// picture must not outlive the read: no file, no cache. A driver asks
	// it with the screen as read, before it captures, and adds no OCR
	// elements when it may not: Android once it has the tree, iOS as soon
	// as it knows the app in front and the lock state, which it asks again
	// with the screen it returns. The gateway sets it on every read, also
	// without OCR: the iOS inspector path takes a picture to place the
	// tree's elements. nil means it may.
	MayCapture func(s *screen.Screen) bool
}

// TapOptions tune Tap.
type TapOptions struct {
	LongPress bool
}

// TypeOptions tune Type.
type TypeOptions struct {
	Append bool // keep the field's current text
	Submit bool // press Enter afterwards
}

// App is an installed app.
type App struct {
	ID     string // package name or bundle id
	Name   string
	System bool
	// Aliases are other names the person knows the app by: on an iPhone,
	// the name its home screen shows when that differs from the app's own
	// ("微信" for WeChat, whose bundle is named WeChat in every language).
	Aliases []string
	// CloneOf is set on an app clone (Android's 应用分身, Dual apps, App
	// Twin): a second copy of an installed app that the phone keeps in
	// another Android user, with its own sign-in and data. The clone's ID
	// is CloneID(package, user) and its Name says it is a clone; CloneOf
	// is the app it copies. Rules, scopes and task app lists judge a clone
	// as that app (Original), since the two read alike on the screen.
	CloneOf *App
}

// Original returns the app as rules judge it: the app a clone copies, or
// a itself.
func (a App) Original() App {
	if a.CloneOf != nil {
		return *a.CloneOf
	}
	return a
}

// CloneID is the ID of the clone of a package in an Android user:
// "com.tencent.mm@999".
func CloneID(pkg string, user int) string {
	return pkg + "@" + strconv.Itoa(user)
}

// ParseCloneID splits a CloneID into its package and user.
func ParseCloneID(id string) (pkg string, user int, ok bool) {
	pkg, u, found := strings.Cut(id, "@")
	if !found || pkg == "" || u == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(u)
	if err != nil || n <= 0 || strconv.Itoa(n) != u {
		return "", 0, false
	}
	return pkg, n, true
}

// Key is a hardware or navigation key.
type Key string

// Keys agents may press. Drivers return ErrUnsupported for keys their
// platform lacks (for example KeyRecents on iPhone).
const (
	KeyBack          Key = "back"
	KeyHome          Key = "home"
	KeyRecents       Key = "recents"
	KeyEnter         Key = "enter"
	KeyTab           Key = "tab"
	KeyDelete        Key = "delete"
	KeyForwardDelete Key = "forward_delete"
	KeyEscape        Key = "escape"
	KeyDpadUp        Key = "dpad_up"
	KeyDpadDown      Key = "dpad_down"
	KeyDpadLeft      Key = "dpad_left"
	KeyDpadRight     Key = "dpad_right"
	KeyPageUp        Key = "page_up"
	KeyPageDown      Key = "page_down"
)

// StreamOptions tune the video stream.
type StreamOptions struct {
	MaxSize int // longest side in pixels; 0 = native
	MaxFPS  int // 0 = device default
}

// Codec is a video codec.
type Codec string

// Codecs.
const (
	H264 Codec = "h264"
	HEVC Codec = "hevc"
)

// Packet is one encoded unit of the video stream.
type Packet struct {
	Data     []byte // Annex B
	PTS      time.Duration
	Config   bool // codec configuration (SPS/PPS/VPS)
	KeyFrame bool
	// Turn is how many quarter turns clockwise put the picture the right
	// way up, 0 to 3. Most streams are upright and leave it 0; an iPhone or
	// iPad on iOS 27 sends its display as the panel is mounted, whatever
	// way the device is turned. Touches go back in the picture's own
	// pixels, before the turn.
	Turn int
}

// Stream is a live encoded video stream.
type Stream interface {
	Codec() Codec
	Size() (width, height int)
	Packets() <-chan Packet // closed when the stream ends; Err says why
	Err() error
	Close() error
}

// Optional capabilities. The gateway checks for them with type assertions.

// TouchAction is a raw touch phase, used for the person's input on the mirror.
type TouchAction string

// Touch actions.
const (
	TouchDown TouchAction = "down"
	TouchMove TouchAction = "move"
	TouchUp   TouchAction = "up"
)

// Toucher injects raw touches, for drags on the mirror. The point is in the
// pixels of the picture the device's Stream sends (at MaxSize 0), because
// that is all the mirror knows. Drivers whose video size differs from their
// screen size (iOS pads its video) convert to screen pixels themselves.
type Toucher interface {
	Touch(ctx context.Context, a TouchAction, p screen.Point) error
}

// PersonWatcher reports input the person makes on the phone itself, so the
// gateway can pause an agent's task when someone picks the phone up. Input
// that Pinky injects must not be reported.
type PersonWatcher interface {
	// PersonInput returns a channel that receives a value, at most a few
	// times a second, while the person touches the screen or presses a key
	// on the phone. It is closed when the device closes.
	PersonInput() <-chan struct{}
}

// InputNoter is an optional Device interface: a note for the agent about
// the last input, which worked but left something unsure, such as typed
// text that could not be read back to check it. TakeInputNote returns the
// note and forgets it; "" when there is none.
type InputNoter interface {
	TakeInputNote() string
}

// LockReporter reports whether the phone is locked.
type LockReporter interface {
	Locked(ctx context.Context) (bool, error)
}

// TextReader is a Device that can read text from the screen's pixels (OCR)
// on this computer when ObserveOptions asks it to, also on screens where
// it has not done so yet. The gateway then checks a tap on a spot the tree
// says little about against the text there (an image button that shows
// "Pay"), before any read has returned OCR elements.
type TextReader interface {
	ReadsText() bool
}

// FrameShooter is a Device whose screenshots are slow where a frame of its
// live video is not: an iPhone over Wi-Fi, whose screenshot is a PNG of up
// to 15 MB. ScreenshotFromFrame returns the screen taken from frame (a
// picture of the live video, as ObserveOptions.Frame gives), cropped to the
// screenshot's size, when the driver prefers it now; ok false means take a
// screenshot. The gateway takes approval cards', history's and agents'
// screenshots this way first.
type FrameShooter interface {
	ScreenshotFromFrame(ctx context.Context, frame func(context.Context) (image.Image, error)) (image.Image, bool)
}

// NotificationLister lists the notifications the phone holds now, with the
// app that posted each. The notification shade shows many of them without
// their app's name (a collapsed row on Android 12 and later), so the
// gateway finds a blocked app's rows there by their text.
type NotificationLister interface {
	Notifications(ctx context.Context) ([]Notification, error)
}

// Notification is one notification the phone holds.
type Notification struct {
	App string // package name or bundle id of the app that posted it
	// Texts are what it says: its title, text, big text, sub text and the
	// like, each as the app set it.
	Texts []string
}

// Clipboard reads and writes the phone's clipboard.
type Clipboard interface {
	SetClipboard(ctx context.Context, text string) error
	Clipboard(ctx context.Context) (string, error)
}

// Settler waits until the screen stops changing after an action, using
// whatever signal the backend has (encoded frame sizes, accessibility
// events). It returns settled=false when max passed first.
type Settler interface {
	WaitSettled(ctx context.Context, max time.Duration) (settled bool, err error)
}

// Waker turns the screen on. Pinky never keeps a phone awake: the gateway
// wakes it before an agent's step, and the phone's own screen timeout and
// lock apply between steps and tasks. Wake must not unlock, press the
// power key (which would lock a phone whose screen is on) or change a
// setting.
type Waker interface {
	// ScreenOn reports whether the display is on.
	ScreenOn(ctx context.Context) (bool, error)
	// Wake turns the display on when it is off, and does nothing when it
	// is on.
	Wake(ctx context.Context) error
}

// Unlocker enters the phone's PIN or passcode on its lock screen. Only the
// person can trigger it, from the client after Touch ID or the system's
// equivalent; agents never reach it and never see the PIN. It wakes the
// screen, brings up the PIN pad, enters pin and confirms, then returns nil
// once the lock screen is gone, ErrLocked when it is still there after the
// PIN was entered (a wrong PIN: one attempt used), or ErrPINNotEntered when
// it stopped before entering anything (the PIN pad never appeared, the
// phone refuses attempts for now). It never retries: phones lock out or
// wipe after repeated failures.
type Unlocker interface {
	Unlock(ctx context.Context, pin string) error
}

// DiskImageDownloader is a Provider that can download the developer disk
// image a computer without Xcode lacks, or whose image Apple's signing
// server refused (iPhones), from a third-party mirror, when the person
// asks.
type DiskImageDownloader interface {
	// DiskImageMirror says where the download comes from.
	DiskImageMirror() DiskImageMirror
	// DownloadDiskImage downloads the image and checks every file;
	// progress, when not nil, gets the bytes done and the total. A file
	// that is not the one expected fails with ErrDiskImageMismatch.
	DownloadDiskImage(ctx context.Context, progress func(done, total int64)) error
}

// DiskImageMirror describes what DiskImageDownloader downloads.
type DiskImageMirror struct {
	Name  string // where it comes from, such as "github.com/doronz88/DeveloperDiskImage"
	Image string // which image, such as "Xcode 27.0 beta 4 (27A5228h)"
	Size  int64  // bytes
}

// ErrDiskImageMismatch is a downloaded developer disk image file that is
// not the one expected (its size or checksum differs); it is not used.
var ErrDiskImageMismatch = errors.New("the downloaded file is not the one expected")

// Checkup items and fixes only iPhones have, with the Check* and Fix*
// names. A failed item's fix can also be a Setup step: SetupDeveloperMode,
// SetupNoDiskImage.
const (
	CheckDeveloperMode = "developer_mode" // Developer Mode is on
	CheckDiskImage     = "disk_image"     // the developer disk image is mounted, and the computer can mount it again after a restart
	CheckHelper        = "helper"         // the WebDriverAgent helper answers, where Pinky drives the phone through it
	FixInstallHelper   = "install_helper" // install the WebDriverAgent helper on the phone
	SetupInstallHelper = FixInstallHelper // also a setup step: install the helper (again) from Xcode, as it is missing or its profile ran out (Info.HelperExpires)
)

// HelperReminder is how long before Info.HelperExpires clients remind the
// person to install the WebDriverAgent helper again.
const HelperReminder = 48 * time.Hour

// FileInfo is one entry of the phone's Pinky folder.
type FileInfo struct {
	Path    string // slash-separated, relative to the Pinky folder
	Size    int64
	ModTime time.Time
	Dir     bool
}

// Files reads and writes the phone's Pinky folder (/sdcard/Pinky on
// Android), the only place on the phone agents may reach. Paths are
// relative to it; drivers refuse paths that leave it (.., absolute paths,
// symlinks out) with ErrNotFound.
type Files interface {
	// ListFiles lists the whole folder, recursively, creating it when it
	// does not exist.
	ListFiles(ctx context.Context) ([]FileInfo, error)
	Pull(ctx context.Context, path string, w io.Writer) error
	// Push writes a file, creating its folders, and sets its time.
	Push(ctx context.Context, path string, r io.Reader, size int64, mtime time.Time) error
}

// AppLogger reads one app's own logs and crash reports, for an agent that
// tests the app (phone_logs). Every line and report comes from the app's
// own processes only; a driver that cannot tell them apart from other
// processes' returns ErrUnsupported rather than more. The gateway decides
// which apps may be read: drivers refuse Pinky's own apps on the phone
// (ErrNotFound).
type AppLogger interface {
	// AppLogs returns the app's log lines since q.Since that pass q.Keep,
	// oldest first. When more than q.Max pass, the newest q.Max are kept
	// and Dropped counts the older ones left out.
	AppLogs(ctx context.Context, q LogQuery) (LogResult, error)
	// AppCrashes returns the app's crash reports since q.Since, newest
	// first, at most q.Max. Filter and Level do not apply.
	AppCrashes(ctx context.Context, q LogQuery) ([]CrashReport, error)
	// DevBuild reports whether the app is a development build: debuggable
	// on Android, signed with get-task-allow on an iPhone.
	DevBuild(ctx context.Context, app App) (bool, error)
}

// LogListener is an AppLogger that only has the lines it collected since
// it was first asked for an app (an iPhone keeps no history a computer can
// read back). StopAppLogs stops collecting and forgets the lines; the
// gateway calls it when the task ends.
type LogListener interface {
	StopAppLogs()
}

// LogLevel is a log line's priority.
type LogLevel string

// Log levels, from the least to the most severe.
const (
	LevelVerbose LogLevel = "verbose"
	LevelDebug   LogLevel = "debug"
	LevelInfo    LogLevel = "info"
	LevelWarn    LogLevel = "warn"
	LevelError   LogLevel = "error"
	LevelFatal   LogLevel = "fatal"
)

// Rank orders levels: 0 for verbose up to 5 for fatal, -1 for none.
func (l LogLevel) Rank() int {
	switch l {
	case LevelVerbose:
		return 0
	case LevelDebug:
		return 1
	case LevelInfo:
		return 2
	case LevelWarn:
		return 3
	case LevelError:
		return 4
	case LevelFatal:
		return 5
	}
	return -1
}

// Letter is the level's letter as logcat writes it: V, D, I, W, E or F.
func (l LogLevel) Letter() string {
	switch l {
	case LevelVerbose:
		return "V"
	case LevelDebug:
		return "D"
	case LevelInfo:
		return "I"
	case LevelWarn:
		return "W"
	case LevelError:
		return "E"
	case LevelFatal:
		return "F"
	}
	return "?"
}

// LogQuery asks AppLogger for one app's lines or crash reports.
type LogQuery struct {
	App   App       // the app as Apps lists it; a clone is read as itself
	Since time.Time // zero means as far back as the phone keeps
	// Filter keeps lines whose tag or text contains it, ignoring case.
	Filter string
	// Level keeps lines of this level or more severe; empty keeps all.
	Level LogLevel
	Max   int // at most this many lines or reports; 0 means no limit
}

// Keep reports whether a line passes the query's filter and level.
func (q LogQuery) Keep(l LogLine) bool {
	if q.Level != "" && l.Level.Rank() < q.Level.Rank() {
		return false
	}
	if q.Filter == "" {
		return true
	}
	f := strings.ToLower(q.Filter)
	return strings.Contains(strings.ToLower(l.Tag), f) || strings.Contains(strings.ToLower(l.Text), f)
}

// LogLine is one line of an app's log.
type LogLine struct {
	Time  time.Time
	PID   int
	Level LogLevel
	Tag   string // Android's tag; an iPhone's subsystem or sender
	Text  string
}

// LogResult is what AppLogs returns.
type LogResult struct {
	Lines   []LogLine // oldest first
	Dropped int       // older lines that passed but were left out
	// From is when the lines start: the time asked for, or later when the
	// phone has nothing older (an iPhone listens from the first call).
	From time.Time
}

// CrashReport is one crash of an app.
type CrashReport struct {
	Time    time.Time
	Kind    string // "crash", "native crash", "ANR" (not responding) or "watchdog" (iPhone: ended for not answering in time)
	Process string
	PID     int
	// Summary is one line: the exception and its message, or the signal.
	Summary string
	Text    string // the report as the phone wrote it, or its main parts
}

// BackgroundOptions configure Backgrounder.OpenBackground.
type BackgroundOptions struct {
	// App is started on the new display once it exists (package, bundle id
	// or display name, as for Launch). Empty starts nothing.
	App string
}

// Backgrounder opens a second display on the phone for an agent to work
// on while the main screen stays free for the person, or off and locked
// (Android: a scrcpy virtual display). The Device it returns observes,
// acts and streams on that display only, and closes it on Close; the
// phone's own Device keeps working on the main screen.
type Backgrounder interface {
	OpenBackground(ctx context.Context, opt BackgroundOptions) (Device, error)
}

// LinkOpener is a Device that opens links: web addresses and app links
// (deep links) such as myapp://item/42. The gateway asks LinkApp which app
// opens a link and judges that app by the rules; OpenLink then opens the
// link in that app and no other, so a link cannot end up in an app the
// rules did not see.
type LinkOpener interface {
	// LinkApp returns the app that opens link on this phone, the one a
	// tap on the link would open without asking the person. Its error
	// wraps ErrNotFound when no app opens the link, and is a
	// *LinkChoiceError when the phone would ask which of several apps
	// should.
	LinkApp(ctx context.Context, link string) (App, error)
	// OpenLink opens link in app, as LinkApp returned it or a clone of it
	// (App.CloneOf), and never in another app.
	OpenLink(ctx context.Context, link string, app App) error
}

// LinkChoiceError is LinkApp's error when several apps open a link and
// the phone would ask the person which: Pinky does not choose for them.
type LinkChoiceError struct {
	Link string
	Apps []App
}

func (e *LinkChoiceError) Error() string {
	names := make([]string, len(e.Apps))
	for i, a := range e.Apps {
		names[i] = a.Name
		if names[i] == "" {
			names[i] = a.ID
		}
	}
	return fmt.Sprintf("several apps open %s: %s", e.Link, strings.Join(names, ", "))
}

// BackgroundDefaulter says whether agents should work on a background
// display unless the person chooses otherwise: true where that display keeps
// working while the phone is off and locked (Android 13 and later), so the
// phone's own screen can stay off during tasks.
type BackgroundDefaulter interface {
	BackgroundByDefault() bool
}

// BackgroundSupporter says whether a driver.Backgrounder works on this
// phone at all (Android 10 cannot read other displays' windows). A
// Backgrounder that does not implement it does.
type BackgroundSupporter interface {
	BackgroundSupported() bool
}

// CompanionState is whether the phone's companion app is ready.
type CompanionState string

// Companion states.
const (
	CompanionOn  CompanionState = "on"  // installed, current and its service is running
	CompanionOff CompanionState = "off" // installed, but its accessibility service is off and Pinky could not turn it on
	// CompanionReady: installed and current; its service is off, and Pinky
	// turns it on while an agent works on the phone (WorkWatcher).
	CompanionReady    CompanionState = "ready"
	CompanionMissing  CompanionState = "missing"  // not installed
	CompanionOutdated CompanionState = "outdated" // installed, older than the one Pinky bundles or signed with another key
	// CompanionPublicKey: installed, but signed with the key of the builds
	// until 2026-11-08, which is public, so anyone could sign an update of
	// it. The bundled build, signed with Pinky's private key, replaces it.
	// Clients treat it as CompanionOutdated and say why.
	CompanionPublicKey CompanionState = "public_key"
	CompanionNoAPK     CompanionState = "no_apk" // Pinky has no APK to install (a build without it)
)

// CompanionInstaller installs or updates the phone's companion app from the
// APK bundled with Pinky. Installing happens only when the person asks in
// the client; the driver never installs anything by itself.
type CompanionInstaller interface {
	CompanionState(ctx context.Context) (CompanionState, error)
	// InstallCompanion installs or updates it and checks that its service
	// comes on, which it then is only while an agent works on the phone.
	// The phone may ask the person to allow the install (Google Play
	// Protect); the call waits for that, up to ctx.
	InstallCompanion(ctx context.Context) error
}

// CompanionStartError is the error InstallCompanion returns when Pinky
// turned the companion app's accessibility service on and it did not
// answer.
type CompanionStartError struct {
	// Reason is what kept it off, as a code clients put into words: one of
	// the CompanionStart* constants, or "" when the phone showed nothing
	// that explains it.
	Reason string
	// Detail says what Pinky saw on the phone, in a sentence, for logs and
	// for clients without words for Reason.
	Detail string
}

func (e *CompanionStartError) Error() string {
	return "the companion's accessibility service did not come on: " + e.Detail
}

// Why the companion's service did not come on, in CompanionStartError.Reason.
const (
	// CompanionStartUIAutomation: another tool holds Android's UI
	// automation (uiautomator, uiautomator2, Appium, Maestro), and Android
	// keeps every accessibility service off until it lets go.
	CompanionStartUIAutomation = "ui_automation"
	// CompanionStartNotUnlocked: the phone has not been unlocked since it
	// started, and Android starts the service only after that.
	CompanionStartNotUnlocked = "not_unlocked"
)

// AppManager installs, updates, uninstalls and stops apps for agents
// (phone_app, docs/notes/apps.md). The gateway asks the person first and
// refuses Pinky's own apps and system apps itself; a driver only does what
// it is told.
type AppManager interface {
	// InspectPackage reads the app the files at paths hold, as the phone
	// would install it: one .apk, the split APKs of one app, one .ipa, or
	// one .app folder. An error wrapping ErrPackage says in plain words why
	// the phone cannot take it (a Simulator build, a profile that does not
	// list the phone).
	InspectPackage(ctx context.Context, paths []string) (Package, error)
	// InstallPackage installs or updates the app the files hold. An error
	// wrapping ErrPackage says in plain words why the phone refused it.
	InstallPackage(ctx context.Context, paths []string) error
	// UninstallApp removes an installed app and its data. id may name an
	// app clone (CloneID), which alone goes.
	UninstallApp(ctx context.Context, id string) error
	// StopApp force-quits an app, as swiping it away does.
	StopApp(ctx context.Context, id string) error
}

// Package is an app in files on the computer, as AppManager reads it.
type Package struct {
	ID      string // package name or bundle id
	Name    string // the name under its icon; ID when it has none
	Version string // "1.3 (14)": the version people see and the build number
	// Signer names who signed it, for the person to recognise: the
	// certificate's common name and the start of its SHA-256 on Android,
	// the team on iPhone.
	Signer string
	// SignerKey identifies the signer exactly: the certificate's SHA-256,
	// or the team id.
	SignerKey string
	// Installed is the version of the app with this id on the phone now;
	// "" when it is not installed.
	Installed string
}

// ErrPackage marks an app the phone cannot take; the error's text says why
// in plain words.
var ErrPackage = errors.New("the phone cannot install this app")

// PackageError returns an error wrapping ErrPackage whose text is the
// plain-words reason alone.
func PackageError(format string, args ...any) error {
	return &packageError{fmt.Sprintf(format, args...)}
}

type packageError struct{ msg string }

func (e *packageError) Error() string        { return e.msg }
func (e *packageError) Is(target error) bool { return target == ErrPackage }

// WorkWatcher is told whether an agent works on the phone: from the start
// of a task to its end, except while the person is in control. A driver
// keeps on, only while one does, what agents need and the phone's apps can
// see: Android's Pinky Companion, whose accessibility service banking apps
// refuse to sign in next to. The gateway calls AgentWorking right after it
// opens the device and whenever the answer changes; it must return at once
// and do any work in the background.
type WorkWatcher interface {
	AgentWorking(on bool)
}

// Pairer pairs phones over the network: Android's wireless debugging, by
// QR code or by pairing code. Paired phones then show up in List with
// Connection "wifi".
type Pairer interface {
	// StartPairing begins pairing by QR code: the client shows QR() as a QR
	// code, the person scans it in Developer options › Wireless debugging ›
	// Pair device with QR code, and Wait returns the phone once it paired
	// and connected.
	StartPairing(ctx context.Context) (Pairing, error)
	// PairWithCode pairs with the address and six-digit code the phone
	// shows under Pair device with pairing code, then connects.
	PairWithCode(ctx context.Context, addr, code string) (Info, error)
}

// Pairing is a QR pairing in progress.
type Pairing interface {
	QR() string // the text the QR code encodes
	Wait(ctx context.Context) (Info, error)
	Close() error
}

// PairingReporter is a Pairing that says how it is going, so a client can
// explain one that takes long.
type PairingReporter interface {
	Progress() PairingProgress
}

// PairingProgress is how far a QR pairing got. The zero value says the
// phone has not shown up yet and nothing is known to be wrong.
type PairingProgress struct {
	// Found is set once the phone showed up on the network, which it does
	// once it scanned the code.
	Found bool
	// Paired is set once the phone took the pairing; the driver then
	// connects to it.
	Paired bool
	// NotAllowed is set while the system does not let Pinky look for
	// phones on the local network (on macOS, Pinky's switch under System
	// Settings › Privacy & Security › Local Network).
	NotAllowed bool
	// SearchErr says why Pinky cannot look for the phone on the network
	// itself, when it cannot; "" while it can.
	SearchErr string
}

// WiFiConnector connects to a phone that is paired with this computer at
// the address it shows under Wireless debugging (IP address & Port), for a
// network on which the driver cannot find it by itself. It returns the
// phone once it is online.
type WiFiConnector interface {
	ConnectWiFi(ctx context.Context, addr string) (Info, error)
}

// Phones away from the computer's network. A Tailnet provider reaches
// phones through Tailscale, the mesh VPN that keeps a person's devices
// connected to each other wherever each one is (the computer at home, the
// phone out on mobile data): it lists the phones on the computer's
// tailnet, connects to one by its Tailscale address, and sets a phone up
// to be reached that way ("away from home").

// ViaTailscale is Info.Via for a phone reached through Tailscale.
const ViaTailscale = "tailscale"

// Tailnet is a Provider that reaches phones through Tailscale.
type Tailnet interface {
	// Tailnet says what Tailscale on this computer reports, and lists the
	// phones on its tailnet.
	Tailnet(ctx context.Context) (TailnetStatus, error)
	// ConnectTailnet connects to a phone on the tailnet at its Tailscale
	// address, with a port or without one (the port Away uses), and
	// returns it once it is online or waits for the person to allow this
	// computer. A phone that does not listen there gives an error
	// wrapping ErrNotListening.
	ConnectTailnet(ctx context.Context, addr string) (Info, error)
	// Away says whether a phone is set up to be reached through
	// Tailscale, and what is missing when it cannot be.
	Away(ctx context.Context, id string) (Away, error)
	// SetAway sets a phone up to be reached through Tailscale, or stops
	// that. On (Android) makes the phone's adb listen on the network, on
	// every network the phone joins, until the phone restarts; the
	// provider then connects to it through Tailscale whenever it can.
	// Off makes adb stop listening and forgets the phone's address: a
	// phone away is then out of reach until it is back.
	SetAway(ctx context.Context, id string, on bool) (Away, error)
}

// TailnetStatus is Tailscale on this computer, and the phones on its
// tailnet.
type TailnetStatus struct {
	// State is one of the Tailnet* states.
	State string
	// Detail is Tailscale's own words when it is not running.
	Detail string
	// Tailnet is the tailnet's name, such as "example.com" or
	// "someone@gmail.com"; Self this computer's name on it.
	Tailnet, Self string
	Phones        []TailnetPhone
}

// Tailscale's states on this computer, in TailnetStatus.State.
const (
	TailnetNotInstalled = "not_installed" // no Tailscale on this computer
	TailnetNotRunning   = "not_running"   // installed, but not running (the app is closed)
	TailnetNeedsLogin   = "needs_login"   // not signed in, or its key expired
	TailnetStopped      = "stopped"       // signed in and turned off
	TailnetStarting     = "starting"
	TailnetRunning      = "running"
)

// TailnetPhone is a phone on the tailnet.
type TailnetPhone struct {
	Name string // its name on the tailnet
	Addr string // its Tailscale address
	// Online reports whether Tailscale sees it connected; LastSeen is when
	// it last was, while it is not.
	Online   bool
	LastSeen time.Time
	// ID is the phone Pinky knows it as, "" when Pinky has not seen it
	// connected.
	ID string
	// Connected reports whether Pinky reaches it through Tailscale now;
	// Relayed whether that goes through one of Tailscale's relay servers
	// rather than straight to the phone.
	Connected, Relayed bool
}

// Away is whether a phone can be reached through Tailscale, away from the
// computer's network.
type Away struct {
	// On reports whether the phone is set up for it: the provider connects
	// to it through Tailscale whenever it can.
	On bool
	// Listening reports whether the phone's adb listens on the network
	// now, as far as the provider knows: it stops when the phone restarts.
	Listening bool
	// Addr is the phone's Tailscale address, "" when it is not on the
	// tailnet; Port where its adb listens.
	Addr string
	Port int
	// Missing says what keeps the phone from being set up, one of the
	// Away* values; "" when nothing does.
	Missing string
	// Detail says the same in a sentence.
	Detail string
}

// What keeps a phone from being set up to be reached away from home, in
// Away.Missing.
const (
	AwayNoTailscaleHere  = "tailscale_here"  // Tailscale does not run on this computer
	AwayNoTailscaleThere = "tailscale_phone" // Tailscale is not connected on the phone, as when another VPN is on
	AwayOtherTailnet     = "tailscale_other" // the phone's Tailscale is on another tailnet than this computer's
	AwayNotConnected     = "not_connected"   // the phone is not connected to set it up (cable or Wi-Fi)
)

// ErrNotListening is wrapped when a phone does not take connections at
// the address and port: its adb does not listen on the network.
var ErrNotListening = errors.New("the phone's adb does not listen on the network there")

// WiFiSwitch is a Provider that moves a phone plugged in by cable to
// Wi-Fi without Wireless debugging (which Android 10 lacks): the phone's
// adb listens on the network (adb tcpip) until the phone restarts, and the
// provider connects to it at its Wi-Fi address, and again by itself
// whenever it is back on the network. The person turns it on for each
// phone, unlike WiFiFinder, which finds iPhones on Wi-Fi by itself.
type WiFiSwitch interface {
	// PhoneWiFi says whether a phone is on Wi-Fi this way, and what keeps
	// it from being moved there.
	PhoneWiFi(ctx context.Context, id string) (WiFiState, error)
	// SetPhoneWiFi moves a phone to Wi-Fi, over its cable, or stops that:
	// its adb then stops listening on the network, which also ends its use
	// away from home (Tailnet), and the provider forgets its addresses.
	SetPhoneWiFi(ctx context.Context, id string, on bool) (WiFiState, error)
}

// WiFiState is whether a phone is moved to Wi-Fi without Wireless
// debugging.
type WiFiState struct {
	// On reports whether it is: the provider reaches the phone at Addr.
	On bool
	// Connected reports whether it is connected there now.
	Connected bool
	// Listening reports whether the phone's adb listens on the network now,
	// as far as the provider knows.
	Listening bool
	// Stopped is set while On but the phone's adb no longer listens, as
	// after the phone restarted: plug it in and turn it on again.
	Stopped bool
	// Addr is the phone's Wi-Fi address; Port where its adb listens.
	Addr string
	Port int
	// Away reports whether the phone is used away from home too (Tailnet),
	// which turning this off ends.
	Away bool
	// Missing says what keeps it from being turned on, one of the WiFi*
	// values; "" when nothing does. Detail says the same in a sentence.
	Missing string
	Detail  string
}

// What keeps a phone from being moved to Wi-Fi, in WiFiState.Missing.
const (
	WiFiNeedsCable = "cable"   // the phone is not plugged in
	WiFiNoWiFi     = "no_wifi" // the phone is not on Wi-Fi
)

// Forgetter is a Provider that connects to phones by itself, as Android's
// reconnects the phones paired over Wi-Fi. The gateway tells it which
// phones the person removed from Pinky, so it leaves them alone: Forget
// when the person removes one and, before the first List, for each one
// removed earlier; Remember once they add it back, by pairing it again or
// plugging it in.
type Forgetter interface {
	// Forget stops the provider from connecting to the phone by itself.
	Forget(id string)
	// Remember undoes Forget.
	Remember(id string)
	// Disconnect drops the phone's connections over the network that the
	// provider can drop, as the person removes it. A cable's connection
	// stays, and the phone with it.
	Disconnect(ctx context.Context, id string) error
}

// WiFiFinder is a Provider that also reaches phones over Wi-Fi by itself:
// iPhones and iPads that usbmuxd lists over the network. Those include
// phones that are not the person's, such as a family member's iPad that was
// once paired with this computer, so the provider leaves a phone it reaches
// only over Wi-Fi alone until the gateway says the phone is the person's:
// it lists that phone with SetupConfirmWiFi, having read no more than its
// name, model and OS version, and neither prepares nor opens it. A phone
// on a cable is always prepared and opened, as before. A phone the gateway
// told it to Forget (Forgetter) it does not list over Wi-Fi at all.
type WiFiFinder interface {
	// SetWiFi turns using phones over Wi-Fi on or off; off, the provider
	// lists phones on a cable only. It starts off.
	SetWiFi(on bool)
	WiFiUser
}

// WiFiUser is a Provider that waits for the gateway to say a phone it
// reaches only over the network is the person's (WiFiFinder; Android's,
// for a phone that Pinky has no identity mark of yet), listing it with
// SetupConfirmWiFi until then.
type WiFiUser interface {
	// UseOverWiFi says a phone is the person's: Pinky used it before, it
	// was on a cable, or the person chose to use it. The provider then
	// prepares and opens it over Wi-Fi too.
	UseOverWiFi(id string)
}

// PhoneTruster is a Provider that checks that a phone reached over the
// network is the one Pinky set up under its id, and lists one that is not
// with SetupPhoneChanged.
type PhoneTruster interface {
	// TrustPhone takes the phone now listed under id as the person's, as
	// the person chose on its page after resetting it or pairing it again:
	// Pinky marks it anew and opens it.
	TrustPhone(ctx context.Context, id string) error
}

// DeviceServiceRestarter is a Provider whose phones go through a service of
// the computer that it can restart when that service lost a phone
// (SetupDeviceService): usbmuxd on a Mac. The system asks an administrator
// to allow it, and every phone of the platform, also on a cable,
// disconnects for a moment.
type DeviceServiceRestarter interface {
	// RestartDeviceService restarts the service. reason is shown in the
	// system's request for an administrator's password. It returns
	// ErrCanceled when the person declines.
	RestartDeviceService(ctx context.Context, reason string) error
}

// ErrCanceled is a request the person declined, such as the system's
// request for an administrator's password.
var ErrCanceled = errors.New("canceled")

// ErrPairedNotFound is wrapped by PairWithCode and Pairing.Wait when the
// phone paired but the driver could not find where to connect to it: its
// wireless debugging service did not show up on the network.
// WiFiConnector then connects with the address the person reads off the
// phone.
var ErrPairedNotFound = errors.New("the phone paired, but its wireless debugging address was not found on the network")

// Why pairing over Wi-Fi failed, wrapped by PairWithCode and Pairing.Wait
// with the pairing tool's own words. adb says the same when the phone
// refused the code and when the connection dropped, so ErrPairRejected
// covers both.
var (
	// ErrPairNotListening: nothing at the address took the connection, as
	// a wrong port, or a pairing dialog or QR screen the phone closed.
	ErrPairNotListening = errors.New("nothing at that address takes pairing requests")
	// ErrPairUnreachable: the address did not answer.
	ErrPairUnreachable = errors.New("the phone did not answer at that address")
	// ErrPairRejected: the phone ended the pairing: a wrong code, a port
	// that is not the pairing dialog's, or a connection that dropped.
	ErrPairRejected = errors.New("the phone ended the pairing")
	// ErrPairOldTool: the pairing tool cannot pair over Wi-Fi, as an adb
	// server older than platform-tools 30.
	ErrPairOldTool = errors.New("this computer's adb server cannot pair over Wi-Fi")
	// ErrPairNoNetwork: the pairing tool cannot reach the local network,
	// which Pinky reaches, as an adb server that lost macOS's Local Network
	// access, and the driver could not restart it. Also wrapped by the
	// driver's other connections at an address: WiFiConnector,
	// ConnectTailnet and the Android driver's own reconnects.
	ErrPairNoNetwork = errors.New("this computer's adb server cannot reach the local network")
)

// ConnectionChooser is a Provider that can reach a phone in more than one
// way and lets the person choose, per phone (iPhone: Xcode's CoreDevice
// protocol, or the WebDriverAgent helper). The choice takes effect at the
// next Open; the gateway closes the phone's session to apply it.
type ConnectionChooser interface {
	// Connections describes the ways to connect a phone, in the order the
	// person sees them, automatic first.
	Connections(ctx context.Context, id string) Connections
	// SetConnection chooses one by its ID; ConnectionAuto goes back to the
	// driver's own rule.
	SetConnection(id, way string) error
}

// ConnectionAuto is the ID of the driver's own choice.
const ConnectionAuto = ""

// Connections is what ConnectionChooser.Connections says about a phone.
type Connections struct {
	Chosen  string       // the person's choice, ConnectionAuto when none
	Current string       // the way the phone is reached now (its Info.Backend), "" when it is not
	Ways    []Connection // automatic first
}

// Connection is one way to connect a phone.
type Connection struct {
	ID        string // ConnectionAuto, or a backend name such as "coredevice" or "wda"
	Available bool   // it can be used with this phone now
	// Reason says why it cannot, as a code clients put into words:
	// "os_version" (the phone's OS is too old or too new for it),
	// "not_installed" (it needs an app on the phone that is not there),
	// "needs_setup" (the phone needs a step first, such as Developer Mode),
	// "offline" (the phone does not answer).
	Reason string
	Detail string // the same in a sentence, for logs and clients without words for Reason
}

// Errors drivers return. Wrap them with fmt.Errorf("...: %w", err).
var (
	ErrUnsupported = errors.New("not supported by this device or backend")
	ErrOffline     = errors.New("device offline")
	ErrLocked      = errors.New("device locked")
	// ErrPINNotEntered: Unlock stopped before entering anything, so no
	// attempt was used.
	ErrPINNotEntered = errors.New("the PIN was not entered")
	ErrNotFound      = errors.New("not found")
)

// Checkups. A phone's checkup checks, item by item and for real, what
// Pinky needs on it (a key that does nothing to see that input works, a
// screenshot, the companion app), and says what to do about each item
// that fails. The client's checkup page and the last step of Add phone
// show it.

// CheckStatus is how one item of a checkup went.
type CheckStatus string

// Check statuses.
const (
	CheckPass CheckStatus = "pass" // it works
	CheckFail CheckStatus = "fail" // it does not work; Fix says what to do
	CheckWarn CheckStatus = "warn" // it works, with a limit the person may want to lift
	CheckSkip CheckStatus = "skip" // not checked: it does not apply, or an earlier item must pass first
)

// Check is one item of a phone's checkup.
type Check struct {
	// ID says what was checked: one of the Check* names below, or one a
	// driver adds (clients show items they do not know by Detail).
	ID     string
	Status CheckStatus
	// Fix names what to do about an item that failed or has a limit, a
	// code clients put into steps and buttons in their own words (the
	// Fix* names below); "" when there is nothing to do.
	Fix string
	// Detail is what the driver found, in its own words and in English:
	// an error, or a fact such as "adb server version 41".
	Detail string
}

// What a checkup checks, in Check.ID.
const (
	CheckHost         = "host"          // the computer has what the phone's platform needs (driver.HostChecker): Fix is what it lacks
	CheckADB          = "adb"           // Android: the adb server answers
	CheckConnection   = "connection"    // the phone is connected and answers
	CheckAuthorized   = "authorized"    // the phone allows this computer (USB debugging allowed, trusted)
	CheckCompanion    = "companion"     // Android: Pinky Companion is installed, current and on
	CheckInput        = "input"         // taps, swipes and keys reach the phone
	CheckScreen       = "screen"        // screenshots and the live picture
	CheckBackground   = "background"    // agents can work on a background display
	CheckWiFi         = "wifi"          // the phone can be paired and used over Wi-Fi
	CheckLocalNetwork = "local_network" // macOS: Pinky may look for phones on the local network
)

// What to do, in Check.Fix.
const (
	FixReconnect        = "reconnect"          // connect the phone again: plug it in, or turn Wi-Fi debugging back on
	FixAuthorize        = "authorize"          // allow this computer on the phone (Allow USB debugging, Trust)
	FixUSBPermission    = "usb_permission"     // Linux: the computer may not open the phone's USB device (udev rules)
	FixUSBDriver        = "usb_driver"         // Windows: the phone's USB debugging has no driver
	FixInstallCompanion = "install_companion"  // install Pinky Companion
	FixUpdateCompanion  = "update_companion"   // update it to the version Pinky carries
	FixTurnOnCompanion  = "turn_on_companion"  // turn its accessibility service on
	FixInputPermission  = "input_permission"   // the phone refuses input from the computer (Xiaomi's USB debugging (Security settings), ColorOS's permission monitoring)
	FixVideoEncoder     = "video_encoder"      // the phone's video encoder fails: the live picture is replaced by still pictures
	FixScreenshot       = "screenshot"         // the phone sends no screenshots
	FixNewerAndroid     = "newer_android"      // the item needs a newer Android version than the phone has
	FixUnlockedOnly     = "unlocked_only"      // background work only while the phone is unlocked (Android 11 and 12)
	FixWirelessDebug    = "wireless_debugging" // turn on Wireless debugging and pair the phone, to use it over Wi-Fi
	FixLocalNetwork     = "local_network"      // allow Pinky under Privacy & Security › Local Network
	FixLocked           = "locked"             // unlock the phone, then check again
	FixRestart          = "restart"            // restart the phone: a setting it has takes effect only then
	FixSameNetwork      = "same_network"       // put the phone on the same Wi-Fi as this computer, one that lets devices reach each other
	FixWiFiSetting      = "wifi_setting"       // turn on Use iPhones and iPads over Wi-Fi in Settings › Phones
)

// Setup steps of Android phones, in Info.Setup with the Setup constants:
// what the computer, not the phone, lacks. A checkup's fix for a phone
// that is not online is its Info.Setup.
const (
	SetupUSBPermission = FixUSBPermission // Linux: no udev rule for the phone, or not in the plugdev group
	SetupUSBDriver     = FixUSBDriver     // Windows: no driver for the phone's USB debugging interface
)

// Checkup is a phone's checkup, as its driver ran it.
type Checkup struct {
	// Family names the maker's system the phone runs, for steps that
	// differ between makers: "xiaomi" (HyperOS, MIUI), "oppo" (ColorOS,
	// OxygenOS, realme UI), "vivo" (OriginOS, Funtouch OS), "honor"
	// (MagicOS), "huawei" (EMUI, HarmonyOS), "samsung" (One UI), "pixel";
	// "" for stock Android and others, such as a Xiaomi phone that runs
	// LineageOS.
	Family string
	// System is that system and its version as the phone names it, for
	// people to read ("HyperOS 2.0", "LineageOS 23.0"); "" when unknown.
	System string
	Checks []Check
}

// Checker is a Device that runs its own checkup. It must not change
// anything on the phone the person would notice: no taps, no settings, no
// installs.
type Checker interface {
	Checkup(ctx context.Context) (Checkup, error)
}
