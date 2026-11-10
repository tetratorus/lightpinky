package ios

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tetratorus/lightpinky/internal/driver"
	"github.com/danielpaulus/go-ios/ios/afc"
)

// Crash reports of one app, from the files crashreportcopymobile shows
// (logs.go). A report is a .ips file named after the process with the time
// it was written, in the phone's own time zone: Example-2026-11-09-101213.ips.
// Its first line is a JSON header naming the app's bundle id; the rest is
// the report, JSON since iOS 15 (bug type 309) and text before (109).

const (
	crashTextMax    = 48 << 10 // bytes of a report's text given back
	crashSummaryMax = 300
	watchdogCode    = 0x8badf00d // "ate bad food": the system killed an app that did not answer in time
)

// crashFiles is the copy service's folder (afcFiles), or a test's.
type crashFiles interface {
	List(dir string) ([]string, error)
	ReadFile(name string, max int64) ([]byte, error)
}

type afcFiles struct{ c *afc.Client }

func (f afcFiles) List(dir string) ([]string, error) { return f.c.List(dir) }

func (f afcFiles) ReadFile(name string, max int64) ([]byte, error) {
	file, err := f.c.Open(name, afc.READ_ONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, max))
}

// readCrashes returns the app's crash reports written since q.Since,
// newest first, at most q.Max of them.
func readCrashes(files crashFiles, b appBundle, q driver.LogQuery) ([]driver.CrashReport, error) {
	if b.Executable == "" {
		return nil, nil
	}
	type candidate struct {
		name string
		at   time.Time
	}
	var cands []candidate
	var listed bool
	var listErr error
	for _, dir := range []string{".", "Retired"} {
		names, err := files.List(dir)
		if err != nil {
			listErr = err
			continue
		}
		listed = true
		for _, n := range names {
			at, ok := crashFileTime(n, b.Executable)
			// The name's time is the phone's local time, read as UTC here:
			// any time zone is within 14 hours of that.
			if !ok || !q.Since.IsZero() && at.Before(q.Since.Add(-14*time.Hour)) {
				continue
			}
			if dir != "." {
				n = dir + "/" + n
			}
			cands = append(cands, candidate{n, at})
		}
	}
	if !listed {
		return nil, fmt.Errorf("crash reports: %w", listErr)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].at.After(cands[j].at) })
	var out []driver.CrashReport
	for i, c := range cands {
		if i >= crashReads || q.Max > 0 && len(out) >= q.Max {
			break
		}
		data, err := files.ReadFile(c.name, crashFileMax)
		if err != nil {
			continue
		}
		r, ok := parseIPS(data, b)
		if !ok || !q.Since.IsZero() && r.Time.Before(q.Since) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// crashFileTime reads the time in a crash report's name, when the name is
// one of exe's reports: exe-2006-01-02-150405[...].ips.
func crashFileTime(name, exe string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, exe+"-")
	const layout = "2006-01-02-150405"
	if !ok || len(rest) < len(layout) || !strings.Contains(rest[len(layout):], ".ips") {
		return time.Time{}, false
	}
	t, err := time.Parse(layout, rest[:len(layout)])
	return t, err == nil
}

// ipsHeader is the first line of a .ips file.
type ipsHeader struct {
	BundleID  string `json:"bundleID"`
	Timestamp string `json:"timestamp"`
	BugType   string `json:"bug_type"`
	AppName   string `json:"app_name"`
	Name      string `json:"name"`
	Version   string `json:"app_version"`
	Build     string `json:"build_version"`
	OS        string `json:"os_version"`
}

// ipsReport is the part of a JSON crash report (bug type 309) Pinky shows.
type ipsReport struct {
	ProcName  string `json:"procName"`
	PID       int    `json:"pid"`
	OSVersion struct {
		Train string `json:"train"`
		Build string `json:"build"`
	} `json:"osVersion"`
	Exception struct {
		Type    string `json:"type"`
		Signal  string `json:"signal"`
		Codes   string `json:"codes"`
		Subtype string `json:"subtype"`
	} `json:"exception"`
	Termination struct {
		Namespace string   `json:"namespace"`
		Code      uint64   `json:"code"`
		Indicator string   `json:"indicator"`
		Reasons   []string `json:"reasons"`
	} `json:"termination"`
	ASI             map[string][]string `json:"asi"`
	ExceptionReason struct {
		Name    string `json:"name"`
		Message string `json:"composed_message"`
	} `json:"exceptionReason"`
	LastExceptionBacktrace []ipsFrame `json:"lastExceptionBacktrace"`
	FaultingThread         int        `json:"faultingThread"`
	Threads                []struct {
		Triggered bool       `json:"triggered"`
		Queue     string     `json:"queue"`
		Name      string     `json:"name"`
		Frames    []ipsFrame `json:"frames"`
	} `json:"threads"`
	UsedImages []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		UUID string `json:"uuid"`
		Base uint64 `json:"base"`
	} `json:"usedImages"`
}

type ipsFrame struct {
	ImageOffset    uint64 `json:"imageOffset"`
	ImageIndex     int    `json:"imageIndex"`
	Symbol         string `json:"symbol"`
	SymbolLocation uint64 `json:"symbolLocation"`
	SourceFile     string `json:"sourceFile"`
	SourceLine     int    `json:"sourceLine"`
}

// parseIPS reads a crash report of the app b, cut down to what tells why
// it crashed: the exception, the reason, the crashed thread's frames and
// the app's own images. Reports of other apps, and reports that are no
// crash (resource use, hangs the app survived), give false.
func parseIPS(data []byte, b appBundle) (driver.CrashReport, bool) {
	first, body, _ := bytes.Cut(data, []byte("\n"))
	var h ipsHeader
	if json.Unmarshal(first, &h) != nil || h.BundleID != b.ID {
		return driver.CrashReport{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05 -0700", h.Timestamp)
	if err != nil {
		return driver.CrashReport{}, false
	}
	r := driver.CrashReport{Time: t, Kind: "crash", Process: h.Name}
	if r.Process == "" {
		r.Process = h.AppName
	}
	switch h.BugType {
	case "309":
		var rep ipsReport
		if err := json.Unmarshal(body, &rep); err != nil {
			// A field of another type than Pinky expects spoils only that
			// field.
			var te *json.UnmarshalTypeError
			if !errors.As(err, &te) {
				return driver.CrashReport{}, false
			}
		}
		if rep.ProcName != "" {
			r.Process = rep.ProcName
		}
		r.PID = rep.PID
		if rep.Termination.Code == watchdogCode {
			r.Kind = "watchdog"
		}
		r.Summary = rep.summary()
		r.Text = rep.text(h, b)
	case "109":
		r.Text = string(body)
		for _, line := range strings.Split(r.Text, "\n") {
			if v, ok := strings.CutPrefix(line, "Exception Type:"); ok {
				r.Summary = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "Process:"); ok && r.PID == 0 {
				// "Process: Example [1234]"
				if i := strings.LastIndex(v, "["); i >= 0 {
					fmt.Sscanf(v[i:], "[%d]", &r.PID)
				}
			}
		}
	default:
		return driver.CrashReport{}, false
	}
	r.Summary = oneLine(r.Summary, crashSummaryMax)
	if len(r.Text) > crashTextMax {
		r.Text = r.Text[:crashTextMax] + "\n…"
	}
	return r, true
}

// summary is the report's reason in one line: the exception's message,
// the app's last words to the system, why it was ended, or the signal.
func (rep *ipsReport) summary() string {
	if e := rep.ExceptionReason; e.Name != "" || e.Message != "" {
		return strings.TrimPrefix(strings.TrimSpace(e.Name+": "+e.Message), ": ")
	}
	for _, lib := range sortedKeys(rep.ASI) {
		for _, s := range rep.ASI[lib] {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	if len(rep.Termination.Reasons) > 0 {
		// "<RBSTerminateContext| domain:10 code:0x8BADF00D explanation:scene-update watchdog transgression: ..."
		r := rep.Termination.Reasons[0]
		if _, why, ok := strings.Cut(r, "explanation:"); ok && strings.TrimSpace(why) != "" {
			return strings.TrimSpace(why)
		}
		return r
	}
	if rep.Termination.Indicator != "" {
		return rep.Termination.Indicator
	}
	s := rep.Exception.Type
	if rep.Exception.Signal != "" {
		s += " (" + rep.Exception.Signal + ")"
	}
	return s
}

// text renders the report's main parts in the order Xcode shows them.
func (rep *ipsReport) text(h ipsHeader, b appBundle) string {
	var w strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&w, "%-16s %s\n", k+":", v)
		}
	}
	proc := rep.ProcName
	if rep.PID > 0 {
		proc = fmt.Sprintf("%s [%d]", proc, rep.PID)
	}
	line("Process", proc)
	line("Identifier", h.BundleID)
	v := h.Version
	if h.Build != "" && h.Build != v {
		v += " (" + h.Build + ")"
	}
	line("Version", v)
	osv := h.OS
	if rep.OSVersion.Train != "" {
		osv = strings.TrimSpace(rep.OSVersion.Train + " (" + rep.OSVersion.Build + ")")
	}
	line("OS Version", osv)
	w.WriteString("\n")
	ex := rep.Exception
	if ex.Type != "" {
		s := ex.Type
		if ex.Signal != "" {
			s += " (" + ex.Signal + ")"
		}
		line("Exception Type", s)
		line("Exception Codes", ex.Codes)
		line("Exception Note", ex.Subtype)
	}
	if t := rep.Termination; t.Namespace != "" || t.Indicator != "" {
		s := t.Namespace
		if t.Code != 0 {
			s = strings.TrimSpace(fmt.Sprintf("%s, Code %#x", s, t.Code))
		}
		if t.Indicator != "" {
			s = strings.TrimPrefix(s+", "+t.Indicator, ", ")
		}
		line("Termination", s)
		for _, r := range t.Reasons {
			line("Reason", r)
		}
	}
	if e := rep.ExceptionReason; e.Name != "" || e.Message != "" {
		line("Exception", strings.TrimPrefix(strings.TrimSpace(e.Name+": "+e.Message), ": "))
	}
	if len(rep.ASI) > 0 {
		w.WriteString("\nApplication Specific Information:\n")
		for _, lib := range sortedKeys(rep.ASI) {
			for _, s := range rep.ASI[lib] {
				w.WriteString(s + "\n")
			}
		}
	}
	if len(rep.LastExceptionBacktrace) > 0 {
		w.WriteString("\nLast Exception Backtrace:\n")
		rep.frames(&w, rep.LastExceptionBacktrace)
	}
	for i, th := range rep.Threads {
		if !th.Triggered && i != rep.FaultingThread {
			continue
		}
		name := th.Name
		if th.Queue != "" {
			name = strings.TrimPrefix(name+"  Dispatch queue: "+th.Queue, "  ")
		}
		fmt.Fprintf(&w, "\nThread %d Crashed:", i)
		if name != "" {
			w.WriteString("  " + name)
		}
		w.WriteString("\n")
		rep.frames(&w, th.Frames)
		break
	}
	var own []string
	for _, im := range rep.UsedImages {
		if inBundle(im.Path, b.Path) || b.Path == "" && im.Name == rep.ProcName {
			name := im.Name
			if name == "" {
				name = path.Base(im.Path)
			}
			own = append(own, fmt.Sprintf("%#x  %s  <%s>", im.Base, name, strings.ReplaceAll(strings.ToLower(im.UUID), "-", "")))
		}
	}
	if len(own) > 0 {
		w.WriteString("\nBinary Images (the app's own):\n")
		for _, s := range own {
			w.WriteString(s + "\n")
		}
	}
	return strings.TrimRight(w.String(), "\n")
}

// frames writes a backtrace, at most crashFrames frames of it.
func (rep *ipsReport) frames(w *strings.Builder, frames []ipsFrame) {
	for i, f := range frames {
		if i == crashFrames {
			fmt.Fprintf(w, "… %d more frames\n", len(frames)-i)
			return
		}
		img, base := "???", uint64(0)
		if f.ImageIndex >= 0 && f.ImageIndex < len(rep.UsedImages) {
			im := rep.UsedImages[f.ImageIndex]
			img, base = im.Name, im.Base
			if img == "" && im.Path != "" {
				img = path.Base(im.Path)
			}
			if img == "" {
				img = "???"
			}
		}
		where := fmt.Sprintf("%#x", base+f.ImageOffset)
		if f.Symbol != "" {
			where += " " + f.Symbol
			if f.SymbolLocation != 0 {
				where += fmt.Sprintf(" + %d", f.SymbolLocation)
			}
		}
		if f.SourceFile != "" {
			where += fmt.Sprintf(" (%s:%d)", f.SourceFile, f.SourceLine)
		}
		fmt.Fprintf(w, "%-3d %-30s %s\n", i, img, where)
	}
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// oneLine joins s into one line of at most max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
