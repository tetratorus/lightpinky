# Handoff: lightpinky

Goal: a minimal Pinky (DIYgod/pinky) for one iPhone on iOS 27+ driven from a Mac, exposing raw phone capabilities over MCP and leaving workflow to the agent ("trust in the agentic consumer"). Native iOS **Require Face ID** is the app lock; no policy engine.

## State (main @ a0faf3b)

- `internal/` is Pinky's `driver`, `driver/ios` (+`cryptex`, `netmux`), `screen`, `ocr`, `video`, `apppkg`, and `driver/android/mdns` (moved to `internal/mdns`), imports rewritten, tests/testdata/iosctl removed, plus 9 files deadcode found fully unreachable. go-ios still replaced by `github.com/DIYgod/go-ios` (adds device-cert pinning).
- `cmd/lightpinky/main.go`: MCP server (modelcontextprotocol/go-sdk v1.8.0), stdio by default, `-http addr` + `LIGHTPINKY_TOKEN` bearer for remote. Tools: devices, download_disk_image, screenshot, elements, tap, swipe, type, key, launch, apps, stop_app, open_link, power. Optional `device` arg; defaults to the only online phone. Reopens once on `driver.ErrOffline`.
- ~33k Go lines (Pinky ~135k non-test).

## Verified

- `GOOS=darwin GOARCH=arm64 go build ./... && go vet ./cmd/...`; linux and windows builds pass.
- On Linux (no usbmuxd): initialize, tools/list (all tools listed), `devices` -> `[]`.

## Not verified: never run against a real iPhone

Run on a Mac with the iPhone plugged in, unlocked, trusted, Developer Mode on:

```sh
go build -o lightpinky ./cmd/lightpinky
scripts/smoke.py ./lightpinky devices
scripts/smoke.py ./lightpinky screenshot elements apps 'power={"wake":true}'
scripts/smoke.py ./lightpinky 'key={"key":"home"}' 'launch={"app":"com.apple.Preferences"}' elements
scripts/smoke.py ./lightpinky 'tap={"x":200,"y":400}' 'swipe={"x1":200,"y1":800,"x2":200,"y2":300}'
```

(Each smoke.py run is a fresh server; the first List may mount the developer disk image and take a while. If `devices` says the image is missing: install Xcode or call `download_disk_image`.) Logs go to stderr.

Things to watch:
- Coordinates: tools assume screenshot pixels == Observe pixels (driver.Device contract). Confirm a tap from `elements` bounds lands.
- `type` uses pasteboard + paste; check it works with a focused field.
- Wi-Fi: `-wifi` (default on) calls `UseOverWiFi` for every listed phone; check unplugging keeps it usable.
- Provider still contains the WDA (iOS 17.4–26) and Simulator paths; `core()` returns ErrUnsupported for stop_app/open_link/power on non-CoreDevice backends.

## Next

1. Phone test above; fix what breaks.
2. Prune further once behavior is known: WDA backend, simulator, Linux netmux, Wi-Fi "lost"/admin restart (`lost.go`, runs `killall usbmuxd` as admin), OCR (unused: ObserveOptions.OCR is false), video streaming if screenshots suffice. Use `GOOS=darwin deadcode ./cmd/lightpinky` after cutting provider branches; deadcode misses darwin cgo-ish files (restored `ocr/bmp.go`) and `apppkg` APK helpers that `appmanage.go` still references.
3. Maybe expose raw `Touch` (down/move/up) and `Drag`; note Touch points are in stream pixels, not screenshot pixels.
