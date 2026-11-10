package ios

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/google/uuid"
)

const (
	displayService = "com.apple.coredevice.displayservice"

	featureStartMediaStream = "com.apple.coredevice.feature.startmediastream"
	featureStopMediaStream  = "com.apple.coredevice.feature.stopmediastream"
	actionMediaStreamStart  = "com.apple.coredevice.action.mediastreamstart"
	actionMediaStreamStop   = "com.apple.coredevice.action.mediastreamstop"

	// errMediaInUse is what the device answers while an app holds the camera
	// or microphone; it refuses to mirror then, as Device Hub does.
	errMediaInUse = 9022
)

// streamAnswer is the part of the device's answer to a stream start that the
// receiver needs. Names are from the device's side: LocalSSRC is the device's
// SSRC and RemoteSSRC is ours.
type streamAnswer struct {
	SessionID   uuid.UUID
	SourcePort  int    // where the device takes RTCP from us
	DeviceSSRC  uint32 // streamConfig.LocalSSRC
	HostSSRC    uint32 // streamConfig.RemoteSSRC
	PayloadType int    // streamConfig.RxPayloadType
	Raw         map[string]any
}

// startVideoStream asks the device to stream one display to recvIP:recvPort.
// The receiver must already be bound: the device sends as soon as it answers.
func startVideoStream(ctx context.Context, svc *cdService, recvIP string, recvPort int, senderIP string, displayID int, o offerOptions) (streamAnswer, error) {
	sessionID := uuid.New()
	offer, err := buildOffer(uuid.New(), rand.Uint32(), o)
	if err != nil {
		return streamAnswer{SessionID: sessionID}, err
	}
	input := map[string]any{
		"clientSupportedFeatures": uint64(140),
		"direction":               "output",
		"negotiatorOffer":         offer,
		"options": map[string]any{
			"AVCMediaStreamNegotiatorAccessNetworkType":     map[string]any{"int": int64(1)},
			"AVCMediaStreamNegotiatorTransportProtocolType": map[string]any{"int": int64(2)},
			"CoreDeviceVideoDisplayMode":                    map[string]any{"string": "DisplayByID"},
			"VideoStreamForDisplayID":                       map[string]any{"int": int64(displayID)},
			"avcMediaStreamOptionClientSessionID":           map[string]any{"uuid": sessionID},
		},
		"receiverIP":   recvIP,
		"receiverPort": uint64(recvPort),
		"senderIP":     senderIP,
		"timeout":      uint64(10), // the device's own deadline, under ours
		"type":         "video",
	}
	out, err := svc.invoke(ctx, featureStartMediaStream, actionMediaStreamStart, input)
	if err != nil {
		var ce *CoreDeviceError
		if errors.As(err, &ce) && ce.Code == errMediaInUse {
			return streamAnswer{SessionID: sessionID}, fmt.Errorf("an app is using the camera or microphone, and iOS does not allow mirroring then: %w", err)
		}
		return streamAnswer{SessionID: sessionID}, err
	}
	a := streamAnswer{SessionID: sessionID, Raw: out}
	if conn, ok := out["connection"].(map[string]any); ok {
		if cfg, ok := conn["streamConfig"].(map[string]any); ok {
			a.SourcePort = int(toInt64(cfg["SourcePort"]))
			a.DeviceSSRC = uint32(toInt64(cfg["LocalSSRC"]))
			a.HostSSRC = uint32(toInt64(cfg["RemoteSSRC"]))
			a.PayloadType = int(toInt64(cfg["RxPayloadType"]))
		}
	}
	return a, nil
}

// stopMediaStreams stops every media stream this host started. Without it the
// device stays "observed remotely", with the screen-sharing indicator on and
// its camera and microphone blocked. The device often drops the channel while
// stopping, which means it worked.
func stopMediaStreams(ctx context.Context, svc *cdService) error {
	_, err := svc.invoke(ctx, featureStopMediaStream, actionMediaStreamStop, map[string]any{"stopAll": true})
	if err != nil && isClosedErr(err) {
		return nil
	}
	return err
}

// stopMediaStreamsFresh stops the streams on a connection of its own, for when
// the one that started them is gone.
func stopMediaStreamsFresh(ctx context.Context, entry goios.DeviceEntry) error {
	svc, err := dialCoreDevice(entry, displayService)
	if err != nil {
		return err
	}
	defer svc.Close()
	return stopMediaStreams(ctx, svc)
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case map[string]any:
		for _, k := range []string{"int", "uint"} {
			if x, ok := n[k]; ok {
				return toInt64(x)
			}
		}
	}
	return 0
}

// udpAddr builds a UDP address for gonet's WriteTo.
func udpAddr(ip string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
}
