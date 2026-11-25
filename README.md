# lightpinky

A native Swift MCP server for this Mac and its paired iPhone: screenshots, accessibility elements, touch, keys, typing and apps. The agent decides how to observe, wait and chain operations. Discover the current schemas with `scripts/phone tools`.

macOS owns pairing, the CoreDevice Wi-Fi tunnel and developer services. The executable uses CoreDevice, RemoteXPC, Foundation and the kernel network stack. There is no Go executable, custom VPN, userspace IP stack or helper installed on the phone.

This build targets an Apple silicon Mac with full Xcode and an iOS 27 iPhone. It uses private APIs from the installed Apple frameworks and may need adjustment after an OS/Xcode update. Simulator support and remote TCP HTTP serving from the former Go implementation are not part of this build.

## Setup

With Xcode selected by `xcode-select` and Python 3.9+:

```sh
gh repo clone tetratorus/lightpinky
cd lightpinky
python3 scripts/setup.py
scripts/phone tools
```

Setup runs `xcrun swift build -c release` and atomically installs `./lightpinky`. It downloads no toolchain or package dependencies. Python is used by the installer and bundled MCP client; phone control runs in the Swift executable.

For an existing checkout, run `git pull --ff-only` and `python3 scripts/setup.py` to update it. Do not overwrite uncommitted changes.

### Connect the phone

First time on a Mac: pair the iPhone in Xcode, tap **Trust**, and enable **Settings › Privacy & Security › Developer Mode**. Once paired, macOS maintains the Wi-Fi connection. The server uses that existing connection.

```sh
scripts/phone devices
```

Discovery uses `devicectl list devices`. The phone must be reachable from this Mac over USB or local Wi-Fi; pairing alone does not provide access from another location. Explicit control acquires a CoreDevice usage assertion and prepares the developer services supplied by Xcode. `download_disk_image` retains its old tool name for compatibility; it prepares Xcode's installed services and does not download a separate disk image.

### Wake, unlock and operate

```sh
scripts/phone 'power={"wake":true}'
scripts/phone unlock 'launch={"app":"Shopee"}' elements screenshot
```

`unlock` reads `IPHONE_UNLOCK_CODE` in the **server's environment**, not a tool argument. The owner configures that variable locally. If it is not already exported, `scripts/phone` loads `~/.zshrc` inside zsh and exports it directly to the server. It never prints the code or puts it in command-line arguments. Do not paste the code into chat or commit it.

Explicit wake/unlock opens the paired developer connection and uses the system passcode screen. Unlock verifies that screen before entering the configured numeric code once. It never retries passcode entry. Face ID approvals and first-time Trust/Developer Mode confirmations still require the person.

`scripts/phone` accepts any tool from `tools`, with arguments as `tool=JSON`. It prints complete text results, saves screenshots to temporary files and prints their paths, and exits nonzero on a failed tool instead of continuing. It builds the binary automatically if missing.

Calls share a background server and its device connections. The server ends the phone's display stream after 30 seconds without stream activity (read-only `power`, `elements`, `apps` and `devices` calls do not keep it open); the next input starts it again. It listens on a private Unix socket under `~/.cache/lightpinky/run/`, restarts when the binary changes, and exits after 15 minutes without calls (`LIGHTPINKY_IDLE=5m` changes that). `scripts/phone stop` ends it; `LIGHTPINKY_DAEMON=0` runs a fresh server for one invocation. `LIGHTPINKY_DEVICE=<UDID>` supplies the default device.

The default phone selection is retained within a server session, avoiding repeated discovery for each command. Calling `devices` refreshes discovery and clears that selection if more than one phone is present. Transport timeouts and disconnects invalidate the connection, including lock-state and streamed app-list timeouts. Power observations, lock-state reads before input, wake's display checks and app-catalog reads retry once on a fresh connection after these failures. Device refusals and malformed results are not retried. A launch can retry its preliminary catalog read, but the launch request itself and all input are sent only once.

Lock-state reads return immediately when the bridge responds. If a read is still pending after one second, its task is canceled and a replacement read starts, with a three-second deadline. An abandoned read cannot supply a result to its replacement or a later call. If the replacement also fails, the connection is invalidated so the read-only caller can reconnect. Cancellation of Apple's async API is cooperative: at most two outstanding lock-state reads per phone are allowed in each process, including across reconnects. If canceled calls have not actually finished, further reads fail explicitly rather than building an unbounded queue; capacity returns when those calls finish. During PIN entry these read retries stay on the same connection; input is never repeated or resumed on a replacement connection.

`lock` ends the developer display session, sends the side button once, and waits for a stable locked state with the screen off. Wake checks only display state, so a slow lock-state query cannot prevent the wake button from being sent. `unlock` wakes the display before checking whether authentication is needed, sends Return once to present the keypad, and waits through transient accessibility errors while it appears. Keypad readiness uses the live lock state already included in the accessibility result, avoiding extra wake and lock-state queries around the same observation. It verifies an empty, focused system passcode field with the complete numeric keypad, and checks live lock state before each digit. The PIN attempt stays on one connection and stops on a read or input failure. After entry, it can reconnect once to confirm the result without entering the code again. A non-empty field or lockout stops the attempt.

Lock and display checks reuse native connections. Lock state and screenshots come from CoreDevice itself (`currentLockState` and the screen-capture service), so they open no extra connection to the phone. Accessibility is the one tool that connects straight to a service port. Reading the phone's service directory displaces macOS's own directory connection; its heartbeat then tends to fail and the tunnel is rebuilt within a couple of minutes. The directory is therefore read at most once. The phone assigns its services one contiguous block of ports in the same order on every tunnel, so the order is saved in `~/.cache/lightpinky/run/layout-<UDID>.json`, and on later tunnels the block is located from the port of a channel this process already holds (seen through `nettop`). The directory is read again only if a port found that way does not answer as the expected service. Fresh accessibility tree pushes can shorten passcode readiness checks; incomplete or old pushes fall back to reading the tree. Unsolicited updates are kept separate from individual element replies.

Developer preparation requests only Developer Mode and DDI services. It avoids the legacy MobileDevice power assertion: `AMDevicePowerAssertionCreate` has been observed hanging over Wi-Fi and invalidating otherwise usable sessions. App launch, URL opening (`open_link`) and app termination (`stop_app`, via `SIGKILL`) use native CoreDevice channels. `type` first writes the phone's pasteboard natively, falling back to `devicectl` if that request fails. The native path preserved the Mac clipboard in live testing. The app catalog is cached for 30 seconds; a lookup miss refreshes it if the cache is at least five seconds old.

For startup and unlock timing diagnostics, run `LIGHTPINKY_DAEMON=0 LIGHTPINKY_TIMING=1 scripts/phone unlock`. Stage and native-request timings go to stderr, including service connection, local tunnel lookup, display, accessibility, lock-state reads and canceled-read replacement. They contain operation names and durations, never passcode or screen content. Unlock queries lock state immediately after PIN input instead of applying a fixed settling delay. Completion still requires a confirmed unlocked state, and input is never retried. Measure the stages before attributing a long total to Wi-Fi or the recorder.

If both lightpinky and `xcrun devicectl device info lockState --device <UDID> --timeout 10` fail with “Client unable to receive tunnel handshake response,” the system tunnel has not opened. In one observed failure, unlocking the phone and restarting the Mac's developer services did not recover it; turning Wi-Fi off and on in the iPhone's Settings did. The original trigger remains unidentified. A successful build or device listing does not verify recovery: check remote lock/unlock and control afterward.

For example, `scripts/phone unlock apps` lists app names and bundle IDs; `scripts/phone unlock 'launch={"app":"Settings"}' elements` opens Settings and reads its screen. Discover coordinates through `elements` or `screenshot` rather than assuming them.

### Use as an MCP server

Run `./lightpinky` as a stdio MCP server in any client. Pass `IPHONE_UNLOCK_CODE` in that process's environment to enable its `unlock` tool. `scripts/phone` is a bundled MCP client, so agents with shell access do not need MCP registration.

The bundled client also uses `-socket PATH -idle 15m` to serve MCP over a Unix socket. The parent directory must belong to the current user with mode 700; the server verifies each client's Unix identity.

### Embed in a Swift application

The package also exports `LightpinkyCore`. `PhoneInfo.discover()` returns paired devices; a `Phone` exposes native `power()` state and `screenshot()` as a `CGImage`. `Server.respond(_:)` accepts JSON-RPC data, including `tools/list` for discovering the full control interface. The same implementation backs the executable and embedded callers.

`videoFrame()` starts the phone's display video stream (RTP/HEVC, decoded with VideoToolbox) and returns its newest frame cropped to the panel, or `nil` when no intact frame is current; fall back to `screenshot()` then. The stream uses far less bandwidth than repeated PNG screenshots, but battery cost has not been measured. It wakes and holds the display active, and iOS refuses to start it during a phone or VoIP call. The phone supports one stream per display; starting another can replace the existing session. The controller shares its frames with the recorder rather than opening a second stream. Stream cleanup uses a stop-all request, including on idle expiry and before locking.

While the controller's display stream is up, it hands one frame a second to a recorder on this Mac through `FrameReceiver` (a private Unix socket the recorder listens on), so the recorder need not pull full screenshots over the link the controller is using. `FrameReceiver.latest(maxAge:)` returns `nil` whenever no controller is streaming. Video frames are not pixel-exact: the phone stretches the picture by two rows and compresses it.

For a mirroring client, `touch(x:y:down:)` streams one finger, `keyChord(_:)` presses raw HID keyboard usages (modifiers included), `orientation()` reports the display rotation (video frames stay in the panel's orientation, coordinates are upright), `pasteboardText()` reads the phone's pasteboard through `devicectl`, and `openURL(_:app:)` opens a link.

Serialize direct `Phone` calls on a worker queue and keep the process's main run loop running: Apple's device services deliver callbacks there. `Server` serializes its own requests and checks for streams idle for 30 seconds every five seconds, in both socket and stdio modes. Direct `Phone.videoFrame()` embedding does not start that timer. Embedding does not start a lightpinky daemon. Discovery uses Apple's `devicectl`; `power()` and `screenshot()` use CoreDevice requests only and never read the phone's service directory.

### Install the agent skill

The portable skill is [.devin/skills/lightpinky-iphone/SKILL.md](.devin/skills/lightpinky-iphone/SKILL.md). Copy it to `~/.config/devin/skills/lightpinky-iphone/SKILL.md` for global use. It points agents back to this repository and these setup instructions; no machine-specific paths or phone IDs are embedded.

## Tools

| Tool | What it does |
|---|---|
| `devices` | Paired iPhones known to macOS CoreDevice |
| `download_disk_image` | Prepare developer services supplied by Xcode |
| `unlock` | Wake and unlock using the server's `IPHONE_UNLOCK_CODE`, including over Wi-Fi |
| `lock` | Send the side button once and verify a stable locked state with the display off; already locked and asleep is a no-op |
| `screenshot` | JPEG of the screen; tap/swipe coordinates are in its pixels. `max_size` shrinks it and says what to multiply image coordinates by. While the display is on, the frame comes from the display video stream: fast on slow Wi-Fi, but compressed and up to 2 pixels off vertically. `video:false` or `png:true` transfers an exact full-size PNG instead; `video:true` fails rather than fall back |
| `elements` | App in front, lock/keyboard state, then one line per element: role, label, id, center `@x,y`, bounds, states. `json` returns the full structure |
| `tap` | Tap or long-press `x`, `y`, or match `find` with optional `role` and zero-based `index` |
| `swipe` | Swipe `x1,y1` → `x2,y2` over `duration_ms` |
| `type` | Type into the focused field (via the pasteboard, any language) |
| `key` | `home`, `back`, `enter`, `delete`, … |
| `launch` | Open an app by bundle id or name |
| `apps` | Installed apps |
| `stop_app` | Quit an app |
| `open_link` | Open a URL in Safari, or deliver it to an explicitly named `app` |
| `power` | Locked / screen on, optionally wake the screen |

Device tools accept a UDID, CoreDevice UUID or exact device name. Omit it when there is one paired iPhone. Actions wait 350 ms after completion unless `no_settle:true`; this is a fixed delay, not a guarantee that an animation has finished.

Accessibility reads the foreground app's native tree through DTX, with bounds converted from points to screenshot pixels. It does not currently merge a separate SpringBoard tree for notification banners or system overlays. Touch and keyboard input require an active developer display session; iOS rejects starting that session during a phone or VoIP call.

## What is not here

The server exposes device capabilities; the agent and owner decide how to use them. iOS authentication and app restrictions still apply.

It can enter a configured passcode on the system unlock screen. It cannot pass Face ID.

## Verify

```sh
swift build -c release
swift test -c release
python3 -m unittest discover -s scripts -p 'test_*.py'
```

## License

Apache-2.0. See [NOTICE](NOTICE).
