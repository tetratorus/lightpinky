# lightpinky development

- Runtime: `Package.swift` and `Sources/`, built with `swift build -c release` using full Xcode on this Apple silicon Mac. No Go sidecars or downloaded toolchains.
- Run `swift test -c release` and `python3 -m unittest discover -s scripts -p 'test_*.py'`. Verify behavior on the paired phone; private Apple API changes require live checks.
- `python3 scripts/setup.py` builds and atomically installs the Swift executable. `scripts/phone tools` discovers the current MCP schemas.
- CoreDevice owns pairing, developer services and the Wi-Fi tunnel. Discovery is passive; explicit wake/unlock may open a paired, locked phone.
- Accessibility resolves `remoteAXService`'s TCP port with a receive-only RSD handshake over macOS's tunnel (`ServiceDirectory.swift`); never send a second host handshake, which rotates and closes service ports on iOS 27.2+.
- `unlock` uses only the server's `IPHONE_UNLOCK_CODE` environment variable. Retry transient connection failures before input, never passcode entry. Never log the code.
- This native build targets the physical iPhone on an Apple silicon Mac with Xcode.
- DTX uses one serialized accessibility request at a time and a background receive loop. Close the session to unblock that loop. Bound peer-controlled frame sizes and walk duration.
- Use `timeIntervalSince1970` or monotonic uptime for comparisons; this Mac's Swift/Foundation combination has a missing symbol for direct `Date <= Date`.
- Keep the portable skill in `.devin/skills/lightpinky-iphone/SKILL.md` as a pointer to the repository README, not a second copy of setup instructions.
