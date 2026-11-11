package ios

import (
	"errors"
	"strings"
	"testing"
)

// A field Pinky cannot read (Xianyu's search box, GitHub issue #76) is
// checked by what the app shows: the text in one more element means it
// went in, the app exactly as before means it did not, and anything else
// stays unknown.
func TestCompareShown(t *testing.T) {
	const app, kb = 10, 20
	el := func(pid, parent int, label, value string) raxElement {
		return raxElement{Object: &axpObject{PID: pid}, Parent: parent, Label: label, Value: value, IsElement: true, Visible: true}
	}
	walk := func(extra ...raxElement) []raxElement {
		els := []raxElement{
			{Object: &axpObject{PID: app, IsApp: true}, Parent: -1},
			el(app, 0, "返回", ""),
			el(app, 0, "搜索宝贝/帖子/用户", ""), // the empty box's placeholder
			el(app, 0, "搜索", ""),
		}
		return append(els, extra...)
	}
	empty := shownFrom(walk(), true)
	// The keyboard's suggestion bar offers the pasted item: not the app.
	offered := shownFrom(walk(el(kb, 0, "Item — Paste from dtpasteboardd", "")), true)
	typed := shownFrom([]raxElement{
		{Object: &axpObject{PID: app, IsApp: true}, Parent: -1},
		el(app, 0, "返回", ""),
		el(app, 0, "奥特曼", ""),
		el(app, 0, "清除文本", ""),
		el(app, 0, "搜索", ""),
	}, true)
	keyboardType := walk(raxElement{Object: &axpObject{PID: app}, Parent: 0, Type: xcKeyboard, IsElement: true, Visible: true, Label: "键盘"})
	keyboardType = append(keyboardType, el(app, len(keyboardType)-1, "粘贴", ""))
	inKeyboard := shownFrom(keyboardType, true)

	for _, tc := range []struct {
		name          string
		before, after shownText
		text          string
		want          shownVerdict
	}{
		{"pasted", empty, typed, "奥特曼", shownSeen},
		{"nothing pasted", empty, offered, "奥特曼", shownUnchanged},
		{"only the keyboard changed", empty, inKeyboard, "奥特曼", shownUnchanged},
		{"changed otherwise", empty, shownFrom(walk(el(app, 0, "热门", "")), true), "奥特曼", shownUnknown},
		{"incomplete walk", empty, shownFrom(walk(), false), "奥特曼", shownUnknown},
		{"the same text again", typed, typed, "奥特曼", shownUnknown},
	} {
		if got := compareShown(tc.before, tc.after, tc.text); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The error for a paste that did not go in names the Allow Paste prompt
// and the setting that turns it off for the app.
func TestNotTakenError(t *testing.T) {
	err := notTakenError("paste", "闲鱼")
	msg := err.Error()
	if !errors.Is(err, errNotTaken) || !strings.Contains(msg, "Allow Paste") || !strings.Contains(msg, "Settings › 闲鱼 › Paste from Other Apps") ||
		!strings.Contains(msg, "phone_handoff") || !strings.Contains(msg, "Don't Allow") {
		t.Fatalf("paste: %v", err)
	}
	if msg := notTakenError("keys", "闲鱼").Error(); strings.Contains(msg, "Allow Paste") || !strings.Contains(msg, "did not go in") {
		t.Fatalf("keys: %v", msg)
	}
}
