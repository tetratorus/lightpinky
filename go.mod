module github.com/tetratorus/lightpinky

go 1.27.3

require (
	github.com/danielpaulus/go-ios v1.3.3-0.20261103131045-6910f3b585ad
	github.com/ebitengine/purego v0.11.3
	github.com/google/uuid v1.6.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	gvisor.dev/gvisor v0.0.0-20240405191320-0878b34101b5
	howett.net/plist v1.0.1
)

require (
	github.com/Masterminds/semver v1.5.0 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/egoist/mygo v0.8.4-0.20261108101141-c8e911e32796 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/miekg/dns v1.1.57 // indirect
	github.com/pierrec/lz4 v2.6.1+incompatible // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.mozilla.org/pkcs7 v0.9.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20230725093048-515e97ebf090 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	software.sslmate.com/src/go-pkcs12 v0.7.2 // indirect
)

tool github.com/egoist/mygo/cmd/mygo

// go-ios's main (6910f3b) plus one commit that pins the phone's TLS certificate to the
// pair record (branch pin-device-certificate of github.com/DIYgod/go-ios). Drop it once
// go-ios has the change; see "go-ios from main" in docs/notes/ios.md.
replace github.com/danielpaulus/go-ios => github.com/DIYgod/go-ios v0.0.0-20261108182015-53a1fc8154fc
