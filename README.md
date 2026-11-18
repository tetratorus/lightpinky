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

Discovery uses `devicectl list devices`. Explicit control acquires a CoreDevice usage assertion and prepares the developer services supplied by Xcode. `download_disk_image` retains its old tool name for compatibility; it prepares Xcode's installed services and does not download a separate disk image.

### Wake, unlock and operate

```sh
scripts/phone 'power={"wake":true}'
scripts/phone unlock 'launch={"app":"Shopee"}' elements screenshot
```

`unlock` reads `IPHONE_UNLOCK_CODE` in the **server's environment**, not a tool argument. The owner configures that variable locally. If it is not already exported, `scripts/phone` loads `~/.zshrc` inside zsh and exports it directly to the server. It never prints the code or puts it in command-line arguments. Do not paste the code into chat or commit it.

Explicit wake/unlock opens the paired developer connection and uses the system passcode screen. Unlock verifies that screen before entering the configured numeric code once. It never retries passcode entry. Face ID approvals and first-time Trust/Developer Mode confirmations still require the person.

`scripts/phone` accepts any tool from `tools`, with arguments as `tool=JSON`. It prints complete text results, saves screenshots to temporary files and prints their paths, and exits nonzero on a failed tool instead of continuing. It builds the binary automatically if missing.

Calls share a background server and its device connections. It listens on a private Unix socket under `~/.cache/lightpinky/run/`, restarts when the binary changes, and exits after 15 minutes without calls (`LIGHTPINKY_IDLE=5m` changes that). `scripts/phone stop` ends it; `LIGHTPINKY_DAEMON=0` runs a fresh server for one invocation. `LIGHTPINKY_DEVICE=<UDID>` supplies the default device.

The default phone selection is retained within a server session, avoiding repeated discovery for each command. Calling `devices` refreshes discovery and clears that selection if more than one phone is present. CoreDevice invalidations and service failures discard cached connections before the next operation; input is never replayed automatically.

`lock` ends the developer display session, sends the side button once, and waits for a stable locked state with the screen off. `unlock` wakes the display before checking whether authentication is needed, verifies an empty system passcode field, and checks live lock state before each digit.

Lock and display checks reuse native connections. Lock state and screenshots come from CoreDevice itself (`currentLockState` and the screen-capture service), so they open no extra connection to the phone. Accessibility is the one tool that connects straight to a service port. Reading the phone's service directory displaces macOS's own directory connection, so the port table is read once per tunnel and saved in `~/.cache/lightpinky/run/ports-<UDID>.json`; it is read again only when a saved port stops answering. Fresh accessibility tree pushes can shorten passcode readiness checks; incomplete or old pushes fall back to reading the tree. Unsolicited updates are kept separate from individual element replies.

Developer preparation requests only Developer Mode and DDI services. It avoids the legacy MobileDevice power assertion: `AMDevicePowerAssertionCreate` has been observed hanging over Wi-Fi and invalidating otherwise usable sessions. App launch also uses the existing native app-service connection instead of starting a separate `devicectl` process.

For startup and unlock timing diagnostics, run `LIGHTPINKY_DAEMON=0 LIGHTPINKY_TIMING=1 scripts/phone unlock`. Stage timings go to stderr; they contain no passcode or screen content.

If both lightpinky and `xcrun devicectl device info lockState --device <UDID> --timeout 10` fail with “Client unable to receive tunnel handshake response,” the system tunnel has not opened. In one observed failure, unlocking the phone and restarting the Mac's developer services did not recover it; turning Wi-Fi off and on in the iPhone's Settings did. The original trigger remains unidentified. A successful build or device listing does not verify recovery: check remote lock/unlock and control afterward.

For example, `scripts/phone unlock apps` lists app names and bundle IDs; `scripts/phone unlock 'launch={"app":"Settings"}' elements` opens Settings and reads its screen. Discover coordinates through `elements` or `screenshot` rather than assuming them.

### Use as an MCP server

Run `./lightpinky` as a stdio MCP server in any client. Pass `IPHONE_UNLOCK_CODE` in that process's environment to enable its `unlock` tool. `scripts/phone` is a bundled MCP client, so agents with shell access do not need MCP registration.

The bundled client also uses `-socket PATH -idle 15m` to serve MCP over a Unix socket. The parent directory must belong to the current user with mode 700; the server verifies each client's Unix identity.

### Embed in a Swift application

The package also exports `LightpinkyCore`. `PhoneInfo.discover()` returns paired devices; a `Phone` exposes native `power()` state and `screenshot()` as a `CGImage`. `Server.respond(_:)` accepts JSON-RPC data, including `tools/list` for discovering the full control interface. The same implementation backs the executable and embedded callers.

`videoFrame()` starts the phone's display video stream (RTP/HEVC, decoded with VideoToolbox) and returns its newest frame cropped to the panel, or `nil` when no intact frame is current; fall back to `screenshot()` then. The stream costs far less bandwidth than repeated PNG screenshots, and iOS refuses to start it during a phone or VoIP call. Each process holds its own stream; only `lock` ends every stream on the phone.

Serialize calls on a worker queue and keep the process's main run loop running: Apple's device services deliver callbacks there. Embedding does not start a lightpinky daemon. Discovery uses Apple's `devicectl`; `power()` and `screenshot()` use CoreDevice requests only and never read the phone's service directory, so a polling embedder does not disturb macOS's connection.

### Install the agent skill

The portable skill is [.devin/skills/lightpinky-iphone/SKILL.md](.devin/skills/lightpinky-iphone/SKILL.md). Copy it to `~/.config/devin/skills/lightpinky-iphone/SKILL.md` for global use. It points agents back to this repository and these setup instructions; no machine-specific paths or phone IDs are embedded.

## Tools

| Tool | What it does |
|---|---|
| `devices` | Paired iPhones known to macOS CoreDevice |
| `download_disk_image` | Prepare developer services supplied by Xcode |
| `unlock` | Wake and unlock using the server's `IPHONE_UNLOCK_CODE`, including over Wi-Fi |
| `lock` | Send the side button once and verify a stable locked state with the display off; already locked and asleep is a no-op |
| `screenshot` | JPEG of the screen; tap/swipe coordinates are in its pixels. `max_size` shrinks it and says what to multiply image coordinates by. `video:true` takes the frame from the display video stream instead of transferring a full PNG |
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
