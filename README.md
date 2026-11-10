# lightpinky

An MCP server that gives an agent an iPhone's raw developer services: screenshots, the accessibility element tree, touch, keys, typing and apps. It is the iOS driver of [Pinky](https://github.com/DIYgod/pinky) with the app, the gateway, the policy engine, Android, telemetry and the updater removed. The agent decides how to observe, wait, retry and chain steps.

No jailbreak and nothing installed on the phone (iOS 27+). It also drives the iOS Simulator. It talks to the same developer services Xcode uses, through [go-ios](https://github.com/danielpaulus/go-ios).

## Setup

On a Mac with Python 3.9+:

```sh
gh repo clone tetratorus/lightpinky
cd lightpinky
python3 scripts/setup.py
scripts/phone tools
```

Setup builds the binary. All Go dependencies are checked into `vendor/`; no Go module downloads are needed. If a suitable Go toolchain is missing, setup downloads the version pinned in `go.mod` from go.dev, checks its published SHA-256, and installs it in your user cache without changing the system. With Go already installed, the build works offline.

For an existing checkout, run `git pull --ff-only` and `python3 scripts/setup.py` to update it. Do not overwrite uncommitted changes.

### Connect the phone

First time on a Mac: plug in the iPhone, unlock it, tap **Trust**, and turn on **Settings › Privacy & Security › Developer Mode**. Those on-device approvals are required once. Afterwards, USB or the same Wi-Fi subnet works (`-wifi=false` disables Wi-Fi for the MCP server).

```sh
scripts/phone devices
```

Discovery waits for Wi-Fi announcements instead of returning an immediate empty list. The result explains any setup still needed. The server mounts Apple's developer disk image automatically when available. If it is missing, use `scripts/phone download_disk_image devices`; this downloads a pinned, checksummed image, so a full Xcode install is not required.

### iOS Simulator

With Xcode installed, booted Simulators are listed by `devices` too (`virtual: true`), next to real phones (`-simulators=false` turns this off). Boot one in Simulator.app or with `xcrun simctl boot <UDID>`. The first time, lightpinky downloads a pinned, checksummed [Appium WebDriverAgent](https://github.com/appium/WebDriverAgent) and builds it with Xcode into your user cache (about a minute); opening a Simulator starts it there. A Simulator has no passcode: `unlock` and the wake option are not needed.

### Wake, unlock and operate

```sh
scripts/phone 'power={"wake":true}'
scripts/phone unlock 'launch={"app":"Shopee"}' elements screenshot
```

`unlock` reads `IPHONE_UNLOCK_CODE` in the **server's environment**, not a tool argument. The owner configures that variable locally. If it is not already exported, `scripts/phone` loads `~/.zshrc` inside zsh and exports it directly to the server. It never prints the code or puts it in command-line arguments. Do not paste the code into chat or commit it.

Locked Wi-Fi phones can be woken and unlocked: `needs_setup: unlock` is not a reason to ask for a cable. These explicit tools open the paired developer connection, wake the display, and use the existing system passcode keypad. A transient connection EOF is retried before input; a passcode attempt is never automatically repeated. Face ID approvals and first-time Trust/Developer Mode confirmations still require the person.

`scripts/phone` accepts any tool from `tools`, with arguments as `tool=JSON`. It prints complete text results, saves screenshots to temporary files and prints their paths, and exits nonzero on a failed tool instead of continuing. It builds the binary automatically if missing.

Calls share one background server, so the phone connection stays open between invocations and each call takes well under a second instead of reconnecting. The server listens on a socket under `~/.cache/lightpinky/run/` that only your user can open, restarts when the binary is rebuilt, and exits after 15 minutes without calls (`LIGHTPINKY_IDLE=5m` changes that). `scripts/phone stop` ends it; `LIGHTPINKY_DAEMON=0` runs a fresh server for just that invocation. `LIGHTPINKY_DEVICE=<UDID>` fills in `device` for every call that takes one.

For example, `scripts/phone unlock apps` lists app names and bundle IDs; `scripts/phone unlock 'launch={"app":"Settings"}' elements` opens Settings and reads its screen. Discover coordinates through `elements` or `screenshot` rather than assuming them.

### Use as an MCP server

Run `./lightpinky` as a stdio MCP server in any client. Pass `IPHONE_UNLOCK_CODE` in that process's environment to enable its `unlock` tool. `scripts/phone` is a bundled MCP client, so agents with shell access do not need MCP registration.

Remote agents: `LIGHTPINKY_TOKEN=... ./lightpinky -http 100.x.y.z:7310` serves MCP at `/mcp` behind a bearer token. Bind it to a private address such as Tailscale's; anyone with the token can drive and, if configured, unlock the phone.

### Install the agent skill

The portable skill is [.devin/skills/lightpinky-iphone/SKILL.md](.devin/skills/lightpinky-iphone/SKILL.md). Copy it to `~/.config/devin/skills/lightpinky-iphone/SKILL.md` for global use. It points agents back to this repository and these setup instructions; no machine-specific paths or phone IDs are embedded.

## Tools

| Tool | What it does |
|---|---|
| `devices` | Phones this Mac sees, their state, and what a phone that is not online waits for |
| `download_disk_image` | Fetch the developer disk image |
| `unlock` | Wake and unlock using the server's `IPHONE_UNLOCK_CODE`, including over Wi-Fi |
| `screenshot` | JPEG of the screen; tap/swipe coordinates are in its pixels. `max_size` shrinks it and says what to multiply image coordinates by |
| `elements` | App in front, lock/keyboard state, then one line per element: role, label, id, center `@x,y`, bounds, states. `json` returns the full structure |
| `tap` | Tap or long-press `x`, `y` |
| `swipe` | Swipe `x1,y1` → `x2,y2` over `duration_ms` |
| `type` | Type into the focused field (via the pasteboard, any language) |
| `key` | `home`, `back`, `enter`, `delete`, … |
| `launch` | Open an app by bundle id or name |
| `apps` | Installed apps |
| `stop_app` | Quit an app |
| `open_link` | Open a URL in the app iOS would open it in |
| `power` | Locked / screen on, optionally wake the screen |

Device tools take an optional `device` (UDID); omit it when only one usable phone is found. Actions (`tap`, `swipe`, `type`, `key`, `launch`, `stop_app`, `open_link`) return once the screen stops changing, for up to 2 s, so the next `elements` or `screenshot` sees the result; `no_settle` returns immediately. Wake/unlock also consider locked Wi-Fi phones. With several phones, pass the desired ID from `devices`.

## What is not here

No blocked apps, approvals or credential-field rules: the agent can do anything a person holding the unlocked phone can. Lock sensitive apps with iOS's own **Require Face ID**. Anything with shell access to this Mac can drive the phone directly anyway.

It can enter a configured passcode on the system unlock screen. It cannot pass Face ID.

## Verify

```sh
GOPROXY=off GOTOOLCHAIN=local go test -mod=vendor ./...
GOPROXY=off GOTOOLCHAIN=local go vet -mod=vendor ./cmd/...
python3 -m unittest discover -s scripts -p 'test_*.py'
```

## License

Apache-2.0. See [NOTICE](NOTICE).
