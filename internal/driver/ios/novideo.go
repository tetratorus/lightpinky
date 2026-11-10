package ios

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// When no picture comes. A stream start that the phone answers but that
// brings no key frame within the wait (restartMedia) fails the open with
// "no video from the device". That alone says nothing of why: whether the
// phone sent nothing at all, or RTCP only, or packets Pinky could not read,
// or frames but never a key frame or parameter sets. The stream counts what
// came (wireStats), and the error carries it with the answer's codec and
// size and the phone's displays (noVideoError.Detail), for the log and the
// phone's checkup, while its message stays short.

// noVideoTry is what one stream start of one display got.
type noVideoTry struct {
	Display int
	Answer  string // the answer's payload type, size and frame rate
	Config  string // the answer's whole stream config, for the log
	Wire    string // what came over the wire
	Err     error  // a start that failed before any wait: why (Answer and Wire are then empty)
}

// noVideoError says that the phone sent no picture Pinky could show from
// any display it asked for.
type noVideoError struct {
	cause    error // the last wait's: context.DeadlineExceeded
	tries    []noVideoTry
	displays string // describeDisplays of the phone's displays
}

// Error names the displays the stream asked for, so that a report tells
// which one gave nothing: "no video from the device (display 1): context
// deadline exceeded".
func (e *noVideoError) Error() string {
	if len(e.tries) == 0 {
		return fmt.Sprintf("no video from the device: %v", e.cause)
	}
	ids := make([]string, len(e.tries))
	for i, t := range e.tries {
		ids[i] = fmt.Sprint(t.Display)
	}
	word := "display"
	if len(ids) > 1 {
		word = "displays"
	}
	return fmt.Sprintf("no video from the device (%s %s): %v", word, strings.Join(ids, ", "), e.cause)
}

func (e *noVideoError) Unwrap() error { return e.cause }

// with returns e with one more try, and that try's cause as the error's;
// a nil e starts a new error, for a phone with displays ds.
func (e *noVideoError) with(t noVideoTry, cause error, ds []displayEntry) *noVideoError {
	if e == nil {
		e = &noVideoError{displays: describeDisplays(ds)}
	}
	e.tries = append(e.tries, t)
	e.cause = cause
	return e
}

// add ends a restart whose last start, of display id, failed with err:
// err itself when no display before it was tried, else e with that try.
func (e *noVideoError) add(err error, id int, ds []displayEntry) error {
	if e == nil {
		return err
	}
	return e.with(noVideoTry{Display: id, Err: err}, err, ds)
}

// Detail is the whole story in one line, for the log and the checkup.
func (e *noVideoError) Detail() string {
	var b strings.Builder
	if e.displays != "" {
		b.WriteString(e.displays)
		b.WriteString(". ")
	}
	for i, t := range e.tries {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "Display %d: ", t.Display)
		if t.Err != nil {
			fmt.Fprintf(&b, "start failed: %v.", t.Err)
			continue
		}
		if t.Answer != "" {
			b.WriteString("answer " + t.Answer + "; ")
		}
		b.WriteString(t.Wire + ".")
	}
	return b.String()
}

// noVideoTry sums up what the stream got, for a wait that timed out.
func (m *media) noVideoTry() noVideoTry {
	m.mu.Lock()
	w, st := m.wire, m.stats
	w.PayloadTypes = cloneCounts(w.PayloadTypes)
	w.NALTypes = cloneCounts(w.NALTypes)
	width, height, cfg := m.width, m.height, m.config != nil
	m.mu.Unlock()
	var lastRTP time.Time
	if n := m.lastRTP.Load(); n != 0 {
		lastRTP = time.Unix(0, n)
	}
	return noVideoTry{
		Display: m.display,
		Answer:  answerSummary(m.ans.Raw),
		Config:  answerConfig(m.ans.Raw),
		Wire:    wireSummary(w, st, int(m.rtcpIn.Load()), cfg, width, height, lastRTP, time.Now()),
	}
}

// wireSummary describes what came over the wire: "0 RTP packets, 9
// RTCP", or with packets what they read as and how far they got.
func wireSummary(w wireStats, st mediaStats, rtcp int, config bool, width, height int, lastRTP, now time.Time) string {
	parts := []string{fmt.Sprintf("%d RTP packets (%d bytes)", st.Packets, st.Bytes), fmt.Sprintf("%d RTCP", rtcp)}
	if w.NotRTP > 0 {
		parts = append(parts, fmt.Sprintf("%d not RTP", w.NotRTP))
	}
	if st.Packets == 0 {
		return strings.Join(parts, ", ")
	}
	if !lastRTP.IsZero() {
		parts = append(parts, fmt.Sprintf("the last %v ago", now.Sub(lastRTP).Round(100*time.Millisecond)))
	}
	parts = append(parts, "payload types "+countList(w.PayloadTypes))
	switch {
	case w.Codec == "":
		parts = append(parts, "payloads read as neither HEVC nor H.264")
	case w.Unreadable > 0:
		parts = append(parts, fmt.Sprintf("read as %s after %d unreadable payloads", w.Codec, w.Unreadable))
	default:
		parts = append(parts, "read as "+w.Codec)
	}
	parts = append(parts, fmt.Sprintf("%d access units", w.Units))
	if len(w.NALTypes) > 0 {
		parts = append(parts, "NAL types "+countList(w.NALTypes))
	}
	switch {
	case config && width > 0:
		parts = append(parts, fmt.Sprintf("parameter sets for %dx%d", width, height))
	case config:
		parts = append(parts, "parameter sets without a size")
	default:
		parts = append(parts, "no parameter sets")
	}
	for _, c := range []struct {
		n    int
		what string
	}{
		{st.KeyFrames, "key frames"},
		{w.KeyNoConfig, "key frames before parameter sets"},
		{w.WaitedKey, "frames passed over waiting for a key frame"},
		{w.BadConfig, "unreadable SPS"},
		{st.Lost, "packets lost"},
		{st.Dropped, "frames dropped incomplete"},
		{st.FIRs, "key frame requests"},
	} {
		if c.n > 0 || c.what == "key frames" {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	return strings.Join(parts, ", ")
}

// noteUnit counts a completed access unit and, until the first picture,
// the types of its NAL units.
func (m *media) noteUnit(codec string, nals [][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wire.Units++
	if m.isReady {
		return
	}
	if m.wire.NALTypes == nil {
		m.wire.NALTypes = map[int]int{}
	}
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		t := int(n[0] & 0x1f)
		if codec == "hevc" {
			t = hevcType(n)
		}
		m.wire.NALTypes[t]++
	}
}

// streamConfig is the answer's connection.streamConfig.
func streamConfig(raw map[string]any) map[string]any {
	conn, _ := raw["connection"].(map[string]any)
	cfg, _ := conn["streamConfig"].(map[string]any)
	return cfg
}

// answerSummary names what the answer says of the picture: "payload 100,
// 1136x2448, 60 fps".
func answerSummary(raw map[string]any) string {
	cfg := streamConfig(raw)
	if cfg == nil {
		if raw == nil {
			return ""
		}
		return "without a stream config"
	}
	var parts []string
	if v, ok := cfg["RxPayloadType"]; ok {
		parts = append(parts, fmt.Sprintf("payload %d", toInt64(v)))
	}
	if w, h := toInt64(cfg["CustomWidth"]), toInt64(cfg["CustomHeight"]); w > 0 || h > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", w, h))
	}
	if f := toInt64(cfg["Framerate"]); f > 0 {
		parts = append(parts, fmt.Sprintf("%d fps", f))
	}
	for _, k := range sortedKeys(cfg) {
		if strings.Contains(strings.ToLower(k), "codec") {
			parts = append(parts, k+" "+shortValue(cfg[k]))
		}
	}
	return strings.Join(parts, ", ")
}

// answerConfig is the answer's whole stream config, keys sorted, long
// values cut short: for the log.
func answerConfig(raw map[string]any) string {
	cfg := streamConfig(raw)
	if cfg == nil {
		return ""
	}
	keys := sortedKeys(cfg)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + shortValue(cfg[k])
	}
	return strings.Join(parts, " ")
}

func shortValue(v any) string {
	switch x := v.(type) {
	case []byte:
		if len(x) > 16 {
			return fmt.Sprintf("<%d bytes>", len(x))
		}
		return fmt.Sprintf("%x", x)
	case map[string]any:
		if _, ok := x["int"]; ok {
			return fmt.Sprint(toInt64(x))
		}
		if _, ok := x["uint"]; ok {
			return fmt.Sprint(toInt64(x))
		}
	}
	s := fmt.Sprint(v)
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

// countList writes counts by key, keys in order: "1×500 19×1".
func countList[K uint8 | int](c map[K]int) string {
	keys := make([]K, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d×%d", k, c[k])
	}
	return strings.Join(parts, " ")
}

func cloneCounts[K comparable](c map[K]int) map[K]int {
	if c == nil {
		return nil
	}
	out := make(map[K]int, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}
