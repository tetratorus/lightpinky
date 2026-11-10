# cryptex

Installs Apple's developer disk image on iOS 27 and later as a cryptex, through cryptexd's remote service (`com.apple.security.cryptexd.remote`). It is go-ios's code with one fix.

## Where it comes from

`cryptex.go`, `tss.go` and `personalized_image.go` are copied from [go-ios](https://github.com/danielpaulus/go-ios) `v1.3.3-0.20261103131045-6910f3b585ad`, `ios/imagemounter`. `util.go` holds two small pieces of the same package (`getFileSize`, `logModule`). The tests' captured values, test image and HTTP/2 framing come from go-ios's `cryptex_test.go`. go-ios is under the MIT License, which `LICENSE` here is a copy of; it applies to the copied code.

Changed from go-ios:

- The package clause (`package cryptex`), and a header in each file that says where it comes from.
- `installCryptex` sends each payload at once (`sendPayload`). go-ios first waited on each payload's stream for the device's FileOpenReply. cryptexd on iOS 27.0.1 sends that reply only after the payload, so both sides waited until the connection dropped. pymobiledevice3 sends the payload at once as well, and notes that a device resets a stream held open waiting for the reply. The streams are still opened before the install request, as go-ios does.
- When sending a payload fails, `installRefusal` waits up to 3 s for the install's reply and reports the device's reason in it, if there is one.
- A debug line per payload: its size and how long it took.

Everything else Pinky uses from go-ios's `imagemounter` comes from go-ios itself: Developer Mode, the personalized image below iOS 27.

Two more things stood in the way on the iPhone 13 mini. Both are in Pinky's tunnel, not in this package:

- The RSD connection the host announces itself on must stay open until the phone has taken the announcement (`rsdLinger` in `conn.go`).
- The tunnel that carries the upload asks for a 1280 MTU (`uploadMTU` in `tunnel.go`).

With all three, Pinky's provider mounted Xcode 27.2's image (27B52) on that phone, iOS 27.0.1, in 1.4 s on 2026-11-08, after the image had been taken off.

## Tests

`cryptex_test.go` runs against a fake cryptexd that behaves as iOS 27.0.1 does: it answers each payload only after receiving it, resets finished streams, and keeps its flow-control windows small. With it:

- `MountImage` installs the image.
- go-ios's original `installCryptex`, kept in `upstream_test.go`, waits forever.
- A device that never replies ends `MountImage` once the connection closes, which is how Pinky's mount timeout and lock watch stop a mount (they close the tunnel).
- A device that refuses an install has its reason reported.

## Removing it

Once go-ios sends the payloads without waiting, use `github.com/danielpaulus/go-ios/ios/imagemounter` again in `mount.go`, `checkup.go`, `ddi.go` and `ddi_device_test.go`, and delete this folder.
