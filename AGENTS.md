# lightpinky development

- Runtime: `Package.swift` and `Sources/`, built with `swift build -c release` using full Xcode on this Apple silicon Mac. No Go sidecars or downloaded toolchains.
- Run `swift test -c release` and `python3 -m unittest discover -s scripts -p 'test_*.py'`. Verify behavior on the paired phone; private Apple API changes require live checks.
- `python3 scripts/setup.py` builds and atomically installs the Swift executable. `scripts/phone tools` discovers the current MCP schemas.
- Lock state, display state and exact screenshots are CoreDevice requests. Touch and keys need the display video stream; the phone allows one per display, it wakes the display, and iOS refuses it during a call. The server ends it after 30 idle seconds.
- CoreDevice owns pairing, developer services and the Wi-Fi tunnel. Discovery is passive; explicit wake/unlock may open a paired, locked phone.
- Accessibility connects straight to `remoteAXService`'s TCP port. Do not read the phone's service directory to find it: every read displaces macOS's own directory connection and the tunnel is usually rebuilt within two minutes. The port comes from the saved layout (`layout-<UDID>.json`: services sit in one contiguous block, in the same order on every tunnel) plus the port of a channel this process already holds, seen through `nettop`. `ServiceDirectory.swift` is the last resort when no layout exists or it has failed twice. Never send a second host handshake.
- `unlock` uses only the server's `IPHONE_UNLOCK_CODE` environment variable. Retry transient connection failures before input, never passcode entry. Never log the code.
- This native build targets the physical iPhone on an Apple silicon Mac with Xcode.
- DTX uses one serialized accessibility request at a time and a background receive loop. Close the session to unblock that loop. Bound peer-controlled frame sizes and walk duration.
- Use `timeIntervalSince1970` or monotonic uptime for comparisons; this Mac's Swift/Foundation combination has a missing symbol for direct `Date <= Date`.
- Keep the portable skill in `.devin/skills/lightpinky-iphone/SKILL.md` as a pointer to the repository README, not a second copy of setup instructions.
