// Package ios is Pinky's iOS driver. It talks to iOS 27+ devices over
// CoreDevice (the protocol behind Xcode's Device Hub) through go-ios, with a
// userspace tunnel so nothing needs root. iPhones on iOS 17.4 to 26 go
// through the WebDriverAgent helper when it is installed (WDADevice), and
// otherwise through the developer services that remain without CoreDevice's
// stream and HID (AXDevice); see pathFor and docs/notes/ios.md.
package ios
