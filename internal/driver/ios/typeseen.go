package ios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// When the focused field cannot be read (an app that draws its own text
// field, such as Xianyu's search box: its element has no value, and empty it
// reads as a text with the placeholder), Type compares what the app shows
// before and after typing instead (GitHub issue #76). The text showing up
// in one more element means it went in; nothing changed at all means it did
// not, which for a paste is most likely iOS's Allow Paste prompt: such an
// app reads the pasteboard itself, iOS asks the person first, and nothing
// is pasted unless they allow it. Pinky cannot see that prompt: it is in
// neither the app's tree nor the screenshot. Anything else stays a note for
// the agent, as before.
//
// Only the accessibility tree is compared. OCR would also cover apps that
// show no text in their tree, but a screenshot here would bypass the
// capture gate that decides what Pinky may picture (observe's MayCapture).

// shownText is the text the app in front shows outside the keyboard, from
// one walk: each element's label, value and placeholder joined, counted.
type shownText struct {
	items    map[string]int
	complete bool // the walk reached every element
}

// readShown walks the app in front for shownText; ok is false when
// remoteAXService cannot be asked or the walk failed. Callers hold inputMu.
func (d *Device) readShown(ctx context.Context) (shownText, bool) {
	rax, err := d.raxNow()
	if err != nil {
		return shownText{}, false
	}
	lim := d.walkLimits()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lim.Budget+raxRequestTimeout)
	els, complete, err := rax.walk(wctx, lim)
	cancel()
	if err != nil || len(els) == 0 {
		return shownText{}, false
	}
	return shownFrom(els, complete), true
}

// shownFrom turns a walk into shownText: the elements of the app's own
// process, outside a keyboard. The keyboard's process, and with it the
// suggestion bar that offers to paste, is left out, as it changes when the
// pasteboard does.
func shownFrom(els []raxElement, complete bool) shownText {
	s := shownText{items: map[string]int{}, complete: complete}
	appPID := els[0].Object.PID
	inKeyboard := make([]bool, len(els))
	for i, e := range els {
		if p := e.Parent; p > 0 && p < i {
			inKeyboard[i] = inKeyboard[p]
		}
		if e.Type == xcKeyboard {
			inKeyboard[i] = true
		}
		if i == 0 || inKeyboard[i] || e.Object.PID != appPID || !e.IsElement || !e.Visible {
			continue
		}
		if e.Label == "" && e.Value == "" && e.Placeholder == "" {
			continue
		}
		s.items[e.Label+"\x00"+e.Value+"\x00"+e.Placeholder]++
	}
	return s
}

// showing counts the elements whose label or value holds text.
func (s shownText) showing(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	n := 0
	for k, c := range s.items {
		label, rest, _ := strings.Cut(k, "\x00")
		value, _, _ := strings.Cut(rest, "\x00")
		if strings.Contains(label, text) || strings.Contains(value, text) {
			n += c
		}
	}
	return n
}

// same reports whether s and t show the same text.
func (s shownText) same(t shownText) bool {
	if len(s.items) != len(t.items) {
		return false
	}
	for k, c := range s.items {
		if t.items[k] != c {
			return false
		}
	}
	return true
}

// shownVerdict is what comparing the app before and after typing tells.
type shownVerdict int

const (
	shownUnknown   shownVerdict = iota // something changed, but not to show the text
	shownSeen                          // one more element shows the text
	shownUnchanged                     // the app shows exactly what it did
)

// compareShown tells what typing text did to an app that showed before
// and shows after. Unchanged needs both walks complete, and the text not
// shown before: typing the same text over itself changes nothing either.
func compareShown(before, after shownText, text string) shownVerdict {
	switch {
	case after.showing(text) > before.showing(text):
		return shownSeen
	case before.complete && after.complete && before.showing(text) == 0 && before.same(after):
		return shownUnchanged
	}
	return shownUnknown
}

// shownWait is how long checkUnread watches the app for the text, after
// the paste's own wait: apps redraw a moment after the keys.
const shownWait = 1500 * time.Millisecond

// checkUnread is checkTyped for a field that cannot be read: it compares
// what the app shows now with before (nil when it could not be read before
// typing), as described at the top of this file. Callers hold inputMu.
func (d *Device) checkUnread(ctx context.Context, text string, before *shownText, method string, st *TypeStats) (string, error) {
	const unread = "Pinky could not read the field back to check the text; look at the screen to be sure."
	st.Checked = "unreadable"
	if before == nil {
		return unread, nil
	}
	verdict := shownUnknown
	for deadline := time.Now().Add(shownWait); ; {
		after, ok := d.readShown(ctx)
		if !ok {
			return unread, nil
		}
		if verdict = compareShown(*before, after, text); verdict == shownSeen || !time.Now().Before(deadline) {
			break
		}
		if err := sleepCtx(ctx, 150*time.Millisecond); err != nil {
			return "", err
		}
	}
	switch verdict {
	case shownSeen:
		st.Checked = "shown"
		return "", nil
	case shownUnchanged:
		st.Checked = "unchanged"
		app := "the app"
		if a, ok := d.foregroundApp(ctx); ok && a.Name != "" {
			app = a.Name
		}
		return "", notTakenError(method, app)
	}
	return unread, nil
}

// errNotTaken marks checkUnread's error: the screen did not change.
var errNotTaken = errors.New("type: the screen reads as it did before typing, so the text most likely did not go in")

// notTakenError is checkUnread's error when the app shows exactly what it
// did before typing. A paste names iOS's Allow Paste prompt and where the
// person turns it off for app.
func notTakenError(method, app string) error {
	if method != "paste" {
		return fmt.Errorf("%w: look at the screen, and if the text is not there, tap the field until it shows a cursor or the keyboard, then type again", errNotTaken)
	}
	return fmt.Errorf("%w. Pinky types by pasting, and %s reads pasted text itself, so iOS may be asking the person to Allow Paste, which Pinky cannot see on the screen; unanswered, nothing is pasted. Call phone_handoff and ask the person to tap Don't Allow if the prompt is showing (Pinky has put their own clipboard back, so Allow Paste would paste that), and to set Settings › %s › Paste from Other Apps to Allow; then type again. If no such prompt came up, tap the field until it shows a cursor or the keyboard, then type again", errNotTaken, app, app)
}
