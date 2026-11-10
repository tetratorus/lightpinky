package ios

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// Diagnostics for iosctl and device tests.

// Services opens a tunnel to the device and lists the RemoteXPC services it
// publishes, with a one-line summary of the tunnel.
func Services(ctx context.Context, udid string) ([]string, string, error) {
	l, err := dialLink(ctx, udid, false)
	if err != nil {
		return nil, "", err
	}
	defer l.Close()
	var names []string
	for n, s := range l.rsd.GetServices() {
		names = append(names, fmt.Sprintf("%s (port %d)", n, s.Port))
	}
	sort.Strings(names)
	info := fmt.Sprintf("host %s, device %s, RSD port %d, MTU %d", l.tunnel.HostAddr(), l.tunnel.DeviceAddr(), l.tunnel.RSDPort(), l.tunnel.MTU())
	return names, info, nil
}

// SwipeLatency swipes back and forth n times and returns, for each swipe,
// how long after the touch went down the first changed video frame
// arrived: input, rendering, encoding and transport together. Horizontal
// swipes page the home screen; vertical ones scroll a list such as
// Settings. Measure on a screen that is otherwise still: animated widgets
// make frames that look like changes.
func (d *Device) SwipeLatency(ctx context.Context, n int, vertical bool) ([]time.Duration, error) {
	s := d.screen()
	left := screen.Point{X: s.Width * 8 / 10, Y: s.Height / 2}
	right := screen.Point{X: s.Width * 2 / 10, Y: s.Height / 2}
	if vertical {
		left = screen.Point{X: s.Width / 2, Y: s.Height * 6 / 10}
		right = screen.Point{X: s.Width / 2, Y: s.Height * 4 / 10}
	}
	var out []time.Duration
	for i := 0; i < n; i++ {
		from, to := left, right
		if i%2 == 1 {
			from, to = right, left
		}
		// Start from a still screen: a list keeps scrolling for seconds
		// after a fling.
		if _, err := d.WaitSettled(ctx, 5*time.Second); err != nil {
			return out, err
		}
		m, _, err := d.live(ctx)
		if err != nil {
			return out, err
		}
		var before []int
		for _, f := range m.framesSince(time.Now().Add(-300 * time.Millisecond)) {
			before = append(before, f.size)
		}
		sort.Ints(before)
		base := 1500
		if len(before) > 0 {
			base = before[len(before)/2]
		}
		limit := 2 * base
		if limit < base+2000 {
			limit = base + 2000
		}
		var down time.Time
		err = d.gestureHook(ctx, swipePath(from, to, 300*time.Millisecond), 10*time.Millisecond, func() { down = time.Now() })
		if err != nil {
			return out, err
		}
		if err := sleepCtx(ctx, 1200*time.Millisecond); err != nil {
			return out, err
		}
		for _, f := range m.framesSince(down) {
			if f.size > limit {
				out = append(out, f.at.Sub(down))
				break
			}
		}
	}
	return out, nil
}

// OrientationReport is what the phone says about its screen's orientation,
// for iosctl orient: the main display as CoreDevice describes it, and the
// frontmost app and its first elements as remoteAXService gives them.
type OrientationReport struct {
	Display  map[string]any // the main display's entry of getdisplayinfo
	App      string         // the frontmost app's label
	AppAttrs map[int]any    // the app element's frame (21) and orientation (129), raw
	Elements []OrientationElement
	RAXErr   error // why remoteAXService gave nothing, when it did not
}

// OrientationElement is one element's label and frame, in points.
type OrientationElement struct {
	Label      string
	X, Y, W, H float64
}

// Orientation reads what OrientationReport describes. It only reads.
func (d *Device) Orientation(ctx context.Context) (OrientationReport, error) {
	var r OrientationReport
	d.svcMu.Lock()
	svc := d.info2
	d.svcMu.Unlock()
	disp, err := readDisplay(ctx, svc)
	if err != nil {
		return r, err
	}
	r.Display = disp
	rax, err := d.raxReady(ctx)
	if err != nil {
		r.RAXErr = err
		return r, nil
	}
	app, err := rax.frontmostApp(ctx)
	if err != nil {
		r.RAXErr = err
		return r, nil
	}
	resp, err := rax.request(ctx, axpRequest{
		Type: axpReqAttributes, Client: raxClientType, Object: app,
		Params: map[string]any{"attributes": []int{axpAttrFrame, axpAttrLabel, axpAttrApplicationOrientation}},
	})
	if err != nil {
		r.RAXErr = err
		return r, nil
	}
	ad, _ := resp.Result.(axpDict)
	r.AppAttrs = map[int]any{}
	for k, v := range ad {
		r.AppAttrs[toInt(k)] = v
	}
	r.App, _ = ad[axpAttrLabel].(string)
	els, _, err := rax.walk(ctx, raxLimits)
	if err != nil && len(els) == 0 {
		r.RAXErr = err
	}
	for _, e := range els {
		if !e.HasFrame || e.Label == "" {
			continue
		}
		r.Elements = append(r.Elements, OrientationElement{Label: e.Label, X: e.Frame.X, Y: e.Frame.Y, W: e.Frame.W, H: e.Frame.H})
		if len(r.Elements) == 12 {
			break
		}
	}
	return r, nil
}

// HeaderExtensions returns the different RTP header extensions the video
// stream carried so far, for iosctl orient.
func (d *Device) HeaderExtensions() [][]byte {
	d.mu.Lock()
	m := d.m
	d.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.headerExts()
}
