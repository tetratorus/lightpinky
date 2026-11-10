package ios

import (
	"context"
	"unicode"
)

// iOS's smart insert adds a space when text is pasted next to a word: "Wall"
// and then "paper" with Append read "Wall paper", "Wi" and "-Fi" "Wi -Fi",
// and text pasted between two words gets one on each side ("hello big
// world"). Chinese next to anything gets none, and neither does text that
// already starts or ends with a space. Type pastes, so it would add spaces
// the agent did not type; it takes them off again with the keyboard. On
// iOS 27.0.1 (iPhone 13 mini, Settings' search, 2026-11-08) the cursor ends
// up after the pasted text and any space added after it, and the arrow keys
// move it one character at a time, so Backspace takes off a space added
// after the text, and Left, Backspace and Right one added before it.

// smartSpaceFix is what smart insert added to a paste: a space before the
// text, after it, or both, and the value the field should read.
type smartSpaceFix struct {
	lead, trail bool
	want        string
}

// smartSpaces works out from the field's value before and after pasting text
// whether smart insert added spaces next to it: found when after reads as
// before with the text and such spaces put in somewhere. ok is false when
// the values allow more than one reading that would need other keys or
// leave another value, so the spaces cannot be taken off safely.
func smartSpaces(before, after, text string) (fix smartSpaceFix, found, ok bool) {
	b, a, x := []rune(before), []rune(after), []rune(text)
	if len(x) == 0 || len(a) <= len(b)+len(x) || len(a) > len(b)+len(x)+2 {
		return smartSpaceFix{}, false, true
	}
	// The paste went in at q: a = b[:q] + chunk + b[q:]. Every q lies where
	// a and b share the prefix before it and the suffix after it.
	prefix := 0
	for prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b) && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	for _, c := range []struct{ lead, trail bool }{{true, false}, {false, true}, {true, true}} {
		n := len(x)
		if c.lead {
			n++
		}
		if c.trail {
			n++
		}
		if len(a) != len(b)+n {
			continue
		}
		for q := max(0, len(b)-suffix); q <= min(prefix, len(b)); q++ {
			chunk := a[q : q+n]
			if c.lead && chunk[0] != ' ' || c.trail && chunk[n-1] != ' ' {
				continue
			}
			mid := chunk
			if c.lead {
				mid = mid[1:]
			}
			if c.trail {
				mid = mid[:len(mid)-1]
			}
			if string(mid) != text {
				continue
			}
			// Smart insert adds a space only between the text and a
			// neighbor that is not a space itself.
			if c.lead && (q == 0 || unicode.IsSpace(b[q-1]) || unicode.IsSpace(x[0])) {
				continue
			}
			if c.trail && (q == len(b) || unicode.IsSpace(b[q]) || unicode.IsSpace(x[len(x)-1])) {
				continue
			}
			f := smartSpaceFix{lead: c.lead, trail: c.trail, want: string(b[:q]) + text + string(b[q:])}
			if found && f != fix {
				return smartSpaceFix{}, true, false // two readings: leave it alone
			}
			fix, found = f, true
		}
	}
	return fix, found, true
}

// cursorSteps is how many times the arrow keys must be pressed to move the
// cursor over text: one per character as a person sees it. It counts only
// text whose every code point is a character of its own; ok is false for
// text with combining marks, emoji sequences, variation selectors, flags,
// conjoining jamo or CRLF, whose steps Pinky does not count.
func cursorSteps(text string) (int, bool) {
	n := 0
	for _, r := range text {
		if joins(r) {
			return 0, false
		}
		n++
	}
	return n, true
}

// joins reports whether r can be part of a character with the code points
// around it.
func joins(r rune) bool {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc): // combining marks
		return true
	case r == '\r', r == 0x200C, r == 0x200D: // CR of CRLF, zero-width (non-)joiner
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tones
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators, in pairs for flags
		return true
	case r >= 0xE0020 && r <= 0xE007F: // tags
		return true
	case r >= 0x1100 && r <= 0x11FF, r >= 0xA960 && r <= 0xA97F, r >= 0xD7B0 && r <= 0xD7FF: // conjoining jamo
		return true
	}
	return false
}

// keys are the keys that take the spaces off, with the cursor after the
// paste: Backspace for one after the text, then Left over the text,
// Backspace and Right back for one before it. ok is false when the cursor
// cannot be moved over the text safely (cursorSteps).
func (f smartSpaceFix) keys(text string) ([]keyStroke, bool) {
	var strokes []keyStroke
	if f.trail {
		strokes = append(strokes, keyStroke{usage: keyBackspace})
	}
	if f.lead {
		steps, ok := cursorSteps(text)
		if !ok {
			return nil, false
		}
		for range steps {
			strokes = append(strokes, keyStroke{usage: keyLeft})
		}
		strokes = append(strokes, keyStroke{usage: keyBackspace})
		for range steps {
			strokes = append(strokes, keyStroke{usage: keyRight})
		}
	}
	return strokes, true
}

// undoSmartSpaces takes off the spaces smart insert added to a paste of text
// into field, whose value was before and is after now. It returns what it
// did for TypeStats.Spaces. Callers hold inputMu.
func (d *Device) undoSmartSpaces(ctx context.Context, field *axpObject, before, after, text string) string {
	fix, found, ok := smartSpaces(before, after, text)
	if !found {
		return ""
	}
	if !ok {
		return "kept"
	}
	strokes, ok := fix.keys(text)
	if !ok {
		return "kept"
	}
	if err := d.pressKeys(ctx, strokes); err != nil {
		return "unconfirmed"
	}
	if ok, _ := d.waitValue(ctx, field, func(v string) bool { return v == fix.want }); !ok {
		return "unconfirmed"
	}
	return "removed"
}
