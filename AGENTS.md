# lightpinky development

- All Go dependencies are committed in `vendor/`, including the certificate-pinning go-ios fork. Build with `GOPROXY=off GOTOOLCHAIN=local go build -mod=vendor ./...`; do not introduce local module replacements.
- Run `GOPROXY=off GOTOOLCHAIN=local go test -mod=vendor ./...`, `go vet -mod=vendor ./cmd/...`, and `python3 -m unittest discover -s scripts -p 'test_*.py'`.
- `python3 scripts/setup.py` bootstraps the pinned Go toolchain if needed and builds the executable. `scripts/phone tools` discovers MCP tools without client registration.
- Explicit wake/unlock may open a paired, locked Wi-Fi phone through `ios.Open`; ordinary discovery stays passive. Do not mistake the provider's passive `SetupUnlock` status for an iOS prohibition on remote unlocking.
- `unlock` uses only the server's `IPHONE_UNLOCK_CODE` environment variable. Retry transient connection failures before input, never passcode entry. Never log the code.
- Keep the portable skill in `.devin/skills/lightpinky-iphone/SKILL.md` as a pointer to the repository README, not a second copy of setup instructions.
