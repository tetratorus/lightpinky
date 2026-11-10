package ios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	goios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"
)

// The WebDriverAgent helper is an app the person signs with their own Apple
// ID, and the provisioning profile it is signed with runs out: 7 days after
// Xcode installed it with a free Apple ID, a year with a paid developer
// account. iOS then no longer starts it, and Pinky could only say that the
// helper did not start. So Pinky reads when the profile runs out from the
// list of provisioning profiles installed on the phone (misagent's CopyAll,
// which only reads), reminds the person driver.HelperReminder before
// (Info.HelperExpires), and once it has run out lists the phone as needing
// the helper installed again (driver.SetupInstallHelper).

// misagentService lists, installs and removes the phone's provisioning
// profiles; Pinky only lists them.
const misagentService = "com.apple.misagent"

// profileTimeout bounds reading the phone's profiles.
const profileTimeout = 10 * time.Second

// provisioningProfile is what Pinky reads of a provisioning profile.
type provisioningProfile struct {
	Name       string
	UUID       string
	AppID      string   // the application identifier it grants: "<team>.<bundle id>", or one ending in "*"
	Devices    []string // the UDIDs it runs on
	AllDevices bool     // an enterprise profile, for any device
	Expires    time.Time
	TeamName   string
	TeamID     string
}

// grants reports whether the profile lets the runner run: its application
// identifier names the runner's bundle id, or a wildcard covers it, for
// the team the runner was signed for when that is known.
func (pr provisioningProfile) grants(r wdaRunner) bool {
	team, id, ok := strings.Cut(pr.AppID, ".")
	if !ok || team == "" || r.BundleID == "" {
		return false
	}
	if r.AppID != "" && !strings.HasPrefix(r.AppID, team+".") {
		return false // another team's
	}
	if prefix, wild := strings.CutSuffix(id, "*"); wild {
		return strings.HasPrefix(r.BundleID, prefix)
	}
	return id == r.BundleID
}

// runsOn reports whether the profile names the phone.
func (pr provisioningProfile) runsOn(udid string) bool {
	if pr.AllDevices || udid == "" {
		return true
	}
	for _, d := range pr.Devices {
		if strings.EqualFold(d, udid) {
			return true
		}
	}
	return false
}

// helperProfile picks the profile the helper runs with: one that grants
// it on this phone. Of several (a helper installed again keeps its old
// profile next to the new one), the one that runs out last counts: iOS
// starts the app while any of them lasts.
func helperProfile(profiles []provisioningProfile, r wdaRunner, udid string) (provisioningProfile, bool) {
	var best provisioningProfile
	found := false
	for _, pr := range profiles {
		if !pr.grants(r) || !pr.runsOn(udid) {
			continue
		}
		if !found || pr.Expires.After(best.Expires) {
			best, found = pr, true
		}
	}
	return best, found
}

// helperExpiry reads when the provisioning profile of the helper runs out
// on the phone behind e (its usbmuxd entry). It is zero when no profile on
// the phone grants the helper.
func helperExpiry(ctx context.Context, e goios.DeviceEntry, r wdaRunner) (time.Time, error) {
	profiles, err := readProfiles(ctx, e)
	if err != nil {
		return time.Time{}, err
	}
	pr, ok := helperProfile(profiles, r, e.Properties.SerialNumber)
	if !ok {
		return time.Time{}, nil
	}
	return pr.Expires, nil
}

// readProfiles lists the provisioning profiles installed on the phone,
// leaving out any it cannot read.
func readProfiles(ctx context.Context, e goios.DeviceEntry) ([]provisioningProfile, error) {
	type result struct {
		profiles []provisioningProfile
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := goios.ConnectToService(e, misagentService)
		if err != nil {
			ch <- result{err: fmt.Errorf("provisioning profiles: %w", err)}
			return
		}
		defer conn.Close()
		if nc := conn.Conn(); nc != nil {
			nc.SetDeadline(time.Now().Add(profileTimeout))
		}
		payloads, err := copyProfiles(conn)
		if err != nil {
			ch <- result{err: fmt.Errorf("provisioning profiles: %w", err)}
			return
		}
		var out []provisioningProfile
		for _, b := range payloads {
			if pr, err := parseProfile(b); err == nil {
				out = append(out, pr)
			}
		}
		ch <- result{profiles: out}
	}()
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.profiles, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("provisioning profiles: %w", ctx.Err())
	}
}

// plistConn is a lockdown service connection, as go-ios makes them.
type plistConn interface {
	Send([]byte) error
	Reader() io.Reader
}

// copyProfiles asks misagent for every provisioning profile on the phone,
// each the DER (or BER) of a CMS signed message.
func copyProfiles(c plistConn) ([][]byte, error) {
	codec := goios.NewPlistCodec()
	req, err := codec.Encode(map[string]any{"MessageType": "CopyAll", "ProfileType": "Provisioning"})
	if err != nil {
		return nil, err
	}
	if err := c.Send(req); err != nil {
		return nil, err
	}
	b, err := codec.Decode(c.Reader())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status  int      `plist:"Status"`
		Payload [][]byte `plist:"Payload"`
	}
	if _, err := plist.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("misagent's answer: %w", err)
	}
	if resp.Status != 0 {
		return nil, fmt.Errorf("misagent answered status %d", resp.Status)
	}
	return resp.Payload, nil
}

// parseProfile reads a provisioning profile: the property list its CMS
// signed message carries. The signature is not checked; Pinky only wants
// the dates.
func parseProfile(b []byte) (provisioningProfile, error) {
	content, err := signedContent(b)
	if err != nil {
		return provisioningProfile{}, err
	}
	var raw struct {
		Name                 string         `plist:"Name"`
		UUID                 string         `plist:"UUID"`
		ExpirationDate       time.Time      `plist:"ExpirationDate"`
		ProvisionedDevices   []string       `plist:"ProvisionedDevices"`
		ProvisionsAllDevices bool           `plist:"ProvisionsAllDevices"`
		Entitlements         map[string]any `plist:"Entitlements"`
		TeamName             string         `plist:"TeamName"`
		TeamIdentifier       []string       `plist:"TeamIdentifier"`
	}
	if _, err := plist.Unmarshal(content, &raw); err != nil {
		return provisioningProfile{}, fmt.Errorf("the profile's property list: %w", err)
	}
	if raw.ExpirationDate.IsZero() {
		return provisioningProfile{}, errors.New("the profile has no expiration date")
	}
	appID, _ := raw.Entitlements["application-identifier"].(string)
	pr := provisioningProfile{
		Name: raw.Name, UUID: raw.UUID, AppID: appID,
		Devices: raw.ProvisionedDevices, AllDevices: raw.ProvisionsAllDevices,
		Expires: raw.ExpirationDate.UTC(), TeamName: strings.TrimSpace(raw.TeamName),
	}
	if len(raw.TeamIdentifier) > 0 {
		pr.TeamID = raw.TeamIdentifier[0]
	}
	return pr, nil
}

// errBER is an encoding signedContent cannot read.
var errBER = errors.New("not a CMS signed message")

// berMaxDepth bounds how deep signedContent follows nested encodings.
const berMaxDepth = 16

// signedContent returns the content a CMS signed message (RFC 5652) signs:
// ContentInfo { signedData, [0] SignedData { version, digestAlgorithms,
// EncapsulatedContentInfo { eContentType, [0] OCTET STRING }, ... } }.
// Apple writes profiles in BER with indefinite lengths and the content as
// a constructed OCTET STRING, which encoding/asn1 refuses, so this walks
// the encoding itself.
func signedContent(b []byte) ([]byte, error) {
	ci, _, err := readBER(b, 0)
	if err != nil {
		return nil, err
	}
	// ContentInfo, then its [0], then SignedData.
	sd, err := child(ci, 1, 2, 0)
	if err != nil {
		return nil, err
	}
	if sd, err = child(sd, 0, 0, 16); err != nil {
		return nil, err
	}
	// SignedData's EncapsulatedContentInfo, its [0], the OCTET STRING.
	eci, err := child(sd, 2, 0, 16)
	if err != nil {
		return nil, err
	}
	wrap, err := child(eci, 1, 2, 0)
	if err != nil {
		return nil, err
	}
	oct, err := child(wrap, 0, 0, 4)
	if err != nil {
		return nil, err
	}
	return octets(oct, 0)
}

// berElem is one BER element; content excludes an indefinite length's
// end-of-contents.
type berElem struct {
	class       byte // 0 universal, 2 context-specific
	tag         int
	constructed bool
	content     []byte
}

// child returns the i-th element inside a constructed element, which must
// have the class and tag given.
func child(e berElem, i int, class byte, tag int) (berElem, error) {
	if !e.constructed {
		return berElem{}, errBER
	}
	rest := e.content
	for n := 0; len(rest) > 0; n++ {
		c, r, err := readBER(rest, 0)
		if err != nil {
			return berElem{}, err
		}
		if n == i {
			if c.class != class || c.tag != tag {
				return berElem{}, errBER
			}
			return c, nil
		}
		rest = r
	}
	return berElem{}, errBER
}

// octets returns an OCTET STRING's bytes, joining a constructed one's
// pieces.
func octets(e berElem, depth int) ([]byte, error) {
	if e.class != 0 || e.tag != 4 || depth > berMaxDepth {
		return nil, errBER
	}
	if !e.constructed {
		return e.content, nil
	}
	var out []byte
	for rest := e.content; len(rest) > 0; {
		c, r, err := readBER(rest, depth+1)
		if err != nil {
			return nil, err
		}
		b, err := octets(c, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		rest = r
	}
	return out, nil
}

// readBER reads the element at the start of b and returns it and what
// follows it.
func readBER(b []byte, depth int) (berElem, []byte, error) {
	if len(b) < 2 || depth > berMaxDepth {
		return berElem{}, nil, errBER
	}
	e := berElem{class: b[0] >> 6, constructed: b[0]&0x20 != 0, tag: int(b[0] & 0x1f)}
	i := 1
	if e.tag == 0x1f { // a tag above 30, in base 128
		e.tag = 0
		for {
			if i >= len(b) || i > 4 {
				return berElem{}, nil, errBER
			}
			c := b[i]
			i++
			e.tag = e.tag<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
	}
	if i >= len(b) {
		return berElem{}, nil, errBER
	}
	l := b[i]
	i++
	switch {
	case l < 0x80:
		n := int(l)
		if len(b)-i < n {
			return berElem{}, nil, errBER
		}
		e.content = b[i : i+n]
		return e, b[i+n:], nil
	case l == 0x80: // indefinite: elements until an end-of-contents
		if !e.constructed {
			return berElem{}, nil, errBER
		}
		start, rest := b[i:], b[i:]
		for {
			if len(rest) < 2 {
				return berElem{}, nil, errBER
			}
			if rest[0] == 0 && rest[1] == 0 {
				e.content = start[:len(start)-len(rest)]
				return e, rest[2:], nil
			}
			_, r, err := readBER(rest, depth+1)
			if err != nil {
				return berElem{}, nil, err
			}
			rest = r
		}
	default:
		k := int(l & 0x7f)
		if k > 4 || len(b)-i < k {
			return berElem{}, nil, errBER
		}
		n := 0
		for _, c := range b[i : i+k] {
			n = n<<8 | int(c)
		}
		i += k
		if n < 0 || len(b)-i < n {
			return berElem{}, nil, errBER
		}
		e.content = b[i : i+n]
		return e, b[i+n:], nil
	}
}

// helperRead is where List reads a phone's helper profile again: the
// attach, the helper, and when it last read it.
type helperRead struct {
	key    entryKey
	runner wdaRunner
	at     time.Time
}

// runnerApp is runner with the runner's application identifier, which
// says whose provisioning profiles grant it.
func (p *Provider) runnerApp(e goios.DeviceEntry) (wdaRunner, error) {
	if p.findRunner != nil {
		id, err := p.findRunner(e)
		return wdaRunner{BundleID: id}, err
	}
	return findRunnerApp(e)
}

// readHelper reads when the helper's profile runs out (helperFn in tests).
func (p *Provider) readHelper(ctx context.Context, e goios.DeviceEntry, r wdaRunner) (time.Time, error) {
	if p.helperFn != nil {
		return p.helperFn(ctx, e, r)
	}
	return helperExpiry(ctx, e, r)
}

// withHelper finishes inspecting a phone driven through the helper: when
// its provisioning profile runs out, and, once it has, that the helper
// must be installed again. A profile Pinky cannot read changes nothing.
func (p *Provider) withHelper(ctx context.Context, e goios.DeviceEntry, info driver.Info, r wdaRunner) driver.Info {
	exp, err := p.readHelper(ctx, e, r)
	now := p.clock()
	p.mu.Lock()
	if p.helpers == nil {
		p.helpers = map[string]helperRead{}
	}
	p.helpers[info.ID] = helperRead{key: keyOf(e), runner: r, at: now}
	p.mu.Unlock()
	switch {
	case err != nil:
		p.logger().Info("iphone: could not read the helper's provisioning profile", "device", info.ID, "helper", r.BundleID, "err", err)
		return info
	case exp.IsZero():
		p.logger().Info("iphone: no provisioning profile on the phone grants the helper", "device", info.ID, "helper", r.BundleID)
		return info
	}
	p.logger().Info("iphone: the helper's provisioning profile", "device", info.ID, "helper", r.BundleID, "expires", exp.Local().Format(time.RFC3339))
	info.HelperExpires = exp
	if !now.Before(exp) {
		info.State, info.Setup, info.Detail = driver.StateNeedsSetup, driver.SetupInstallHelper, helperExpired(exp)
	}
	return info
}

// helperRecheck is how often List reads the helper's profile of an online
// phone again: every minute while clients remind the person, so that the
// reminder goes once they installed the helper again, and every half hour
// before.
func helperRecheck(exp, now time.Time) time.Duration {
	if !exp.IsZero() && exp.Sub(now) <= driver.HelperReminder {
		return recheckOnline
	}
	return 30 * time.Minute
}

// helperStill reads again when the helper of a phone List knows as online
// runs out (see helperRecheck), so that a helper installed again while the
// phone stays plugged in is noticed, and reports false once the profile
// has run out: List then inspects the phone again, which says to install
// the helper again.
func (p *Provider) helperStill(ctx context.Context, e goios.DeviceEntry, k known) (driver.Info, bool) {
	id, now := k.info.ID, p.clock()
	if exp := k.info.HelperExpires; !exp.IsZero() && !now.Before(exp) {
		return k.info, false // the inspection reads it again
	}
	p.mu.Lock()
	h, ok := p.helpers[id]
	due := ok && h.key == k.key && now.Sub(h.at) >= helperRecheck(k.info.HelperExpires, now)
	if due {
		h.at = now
		p.helpers[id] = h
	}
	p.mu.Unlock()
	if due {
		if exp, err := p.readHelper(ctx, e, h.runner); err == nil && !exp.Equal(k.info.HelperExpires) {
			p.mu.Lock()
			if cur, still := p.known[id]; still && cur.key == k.key {
				cur.info.HelperExpires = exp
				p.known[id] = cur
			}
			p.mu.Unlock()
			if !exp.IsZero() {
				p.logger().Info("iphone: the helper's provisioning profile changed", "device", id, "helper", h.runner.BundleID, "expires", exp.Local().Format(time.RFC3339))
			}
			k.info.HelperExpires = exp
		}
	}
	exp := k.info.HelperExpires
	return k.info, exp.IsZero() || now.Before(exp)
}

// forgetHelpers drops the helper reads of phones that are gone.
func (p *Provider) forgetHelpers(listed map[string]bool) {
	for id := range p.helpers {
		if !listed[id] {
			delete(p.helpers, id)
		}
	}
}

// helperExpires reads afresh when the provisioning profile of the helper
// the phone runs runs out, for the checkup; zero on a simulator, which
// needs none.
func (d *WDADevice) helperExpires(ctx context.Context) (time.Time, error) {
	if d.helperFn != nil {
		return d.helperFn(ctx)
	}
	if d.entry.Properties.SerialNumber == "" {
		return time.Time{}, nil
	}
	r, err := findRunnerApp(d.entry)
	if err != nil || r.BundleID == "" {
		return time.Time{}, err
	}
	return helperExpiry(ctx, d.entry, r)
}

// helperExpired says that the helper's profile ran out.
func helperExpired(exp time.Time) string {
	return "The WebDriverAgent helper's provisioning profile ran out on " + helperDate(exp) +
		", so iOS no longer starts it. Install it again from Xcode (docs/notes/ios.md, \"Installing the WebDriverAgent helper\"); with a free Apple ID it lasts 7 days."
}

// helperDate writes when a profile runs out, in the computer's time zone.
func helperDate(t time.Time) string { return t.Local().Format("Monday, January 2, 2006 at 15:04 MST") }
