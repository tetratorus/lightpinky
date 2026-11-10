# lightpinky

An MCP server that gives an agent an iPhone's raw developer services: screenshots, the accessibility element tree, touch, keys, typing and apps. It is the iOS driver of [Pinky](https://github.com/DIYgod/pinky) with the app, the gateway, the policy engine, Android, telemetry and the updater removed. The agent decides how to observe, wait, retry and chain steps.

No jailbreak and nothing installed on the phone (iOS 27+). It talks to the same developer services Xcode uses, through [go-ios](https://github.com/danielpaulus/go-ios).

## Setup

1. Plug the iPhone into the Mac, unlock it and tap **Trust**.
2. Turn on **Settings › Privacy & Security › Developer Mode** (the phone restarts).
3. Build and register:

   ```sh
   go build -o lightpinky ./cmd/lightpinky
   claude mcp add lightpinky -- "$PWD/lightpinky"
   ```

The server mounts Apple's developer disk image when the phone needs it. If `devices` says none is found, install Xcode or call `download_disk_image` (a pinned, checksummed mirror).

After the phone has been used on the cable, it keeps working on the same Wi-Fi (`-wifi=false` turns that off).

Remote agents: `LIGHTPINKY_TOKEN=... ./lightpinky -http 100.x.y.z:7310` serves MCP at `/mcp` behind a bearer token. Bind it to a private address such as Tailscale's; anyone with the token can drive the phone.

## Tools

| Tool | What it does |
|---|---|
| `devices` | Phones this Mac sees, their state, and what a phone that is not online waits for |
| `download_disk_image` | Fetch the developer disk image |
| `screenshot` | JPEG of the screen; tap/swipe coordinates are in its pixels |
| `elements` | App in front, lock/keyboard state, every element with role, label, states, bounds |
| `tap` | Tap or long-press `x`, `y` |
| `swipe` | Swipe `x1,y1` → `x2,y2` over `duration_ms` |
| `type` | Type into the focused field (via the pasteboard, any language) |
| `key` | `home`, `enter`, `delete`, … |
| `launch` | Open an app by bundle id or name |
| `apps` | Installed apps |

Every tool takes an optional `device` (UDID); it may be left out when one iPhone is online.

## What is not here

No blocked apps, approvals or credential-field rules: the agent can do anything a person holding the unlocked phone can. Lock sensitive apps with iOS's own **Require Face ID**. Anything with shell access to this Mac can drive the phone directly anyway.

It cannot unlock the phone or pass Face ID.

## License

Apache-2.0. See [NOTICE](NOTICE).
