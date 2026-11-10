package ios

import (
	"strings"
	"unicode/utf8"
)

// VoiceOver captions read "label, value, traits, hint", in the phone's
// language. The walk gets only captions (see ax.go), so roles come from the
// trait words in them. English and Simplified Chinese are known; in other
// languages elements keep their whole caption as the label and the role
// "other".

// traitWords maps spoken trait words to normalized trait names.
var traitWords = map[string]string{
	"button": "button", "按钮": "button",
	"back button": "back button", "返回按钮": "back button",
	"pop up button": "button", "弹出式按钮": "button",
	"link": "link", "链接": "link",
	"image": "image", "图像": "image", "图片": "image",
	"selected": "selected", "已选中": "selected", "已选择": "selected",
	"header": "header", "heading": "header", "标题": "header",
	"search field": "search field", "搜索栏": "search field", "搜索字段": "search field",
	"text field": "text field", "text area": "text field", "文本栏": "text field", "文本字段": "text field", "文本区域": "text field",
	"secure text field": "secure text field", "安全文本栏": "secure text field", "安全文本字段": "secure text field",
	"adjustable": "adjustable", "可调整": "adjustable",
	"toggle": "toggle", "switch button": "toggle", "开关按钮": "toggle", "开关": "toggle",
	"tab": "tab", "标签页": "tab",
	"dimmed": "dimmed", "not enabled": "dimmed", "变暗": "dimmed", "已停用": "dimmed", "灰色": "dimmed",
	"updates frequently": "updates frequently", "频繁更新": "updates frequently",
	"menu item": "menu item", "菜单项": "menu item",
	"keyboard key": "keyboard key", "键盘键": "keyboard key",
	"slider": "adjustable", "滑块": "adjustable",
	"picker item": "picker item", "选取器项目": "picker item",
}

// hintPrefixes start the usage hints VoiceOver appends.
var hintPrefixes = []string{
	"double tap", "double-tap", "triple tap", "activate to", "swipe up or down", "swipe left or right",
	"轻点两下", "连按两次", "激活以", "上下轻扫", "左右轻扫",
}

type captionParts struct {
	label  string   // the caption without trait words and hints
	first  string   // its first part, usually the accessibility label
	traits []string // normalized trait names found
	value  string   // a switch's value, "0" or "1", if present
}

func splitCaption(c string) []string {
	c = strings.ReplaceAll(c, "，", ", ")
	parts := strings.Split(c, ", ")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseCaption(c string) captionParts {
	parts := splitCaption(c)
	var cp captionParts
	var keep []string
	for i, p := range parts {
		l := strings.ToLower(strings.TrimSuffix(p, "."))
		if i > 0 {
			if t, ok := traitWords[l]; ok {
				cp.traits = append(cp.traits, t)
				continue
			}
			hint := false
			for _, h := range hintPrefixes {
				if strings.HasPrefix(l, h) {
					hint = true
					break
				}
			}
			if hint {
				continue
			}
		}
		keep = append(keep, p)
	}
	if has(cp.traits, "toggle") && len(keep) > 1 {
		switch strings.ToLower(keep[len(keep)-1]) {
		case "0", "1", "on", "off", "开", "关":
			cp.value = keep[len(keep)-1]
			keep = keep[:len(keep)-1]
		}
	}
	cp.label = strings.Join(keep, ", ")
	if len(keep) > 0 {
		cp.first = keep[0]
	}
	return cp
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// keyboardRows are runs of keys that only an on-screen keyboard shows in a
// row: the walk reads a keyboard as one element per key.
var keyboardRows = []string{"qwertyuiop", "asdfghjkl", "zxcvbnm", "azertyuiop", "qwertzuiop"}

// keyboardKeys finds the walk positions of on-screen keyboard keys: a run
// of single-letter elements that spells a keyboard row, and everything
// after it that belongs to the keyboard (letters, shift, delete, space,
// return, the number key and the globe).
func keyboardKeys(labels []string) map[int]bool {
	keys := map[int]bool{}
	for start := 0; start < len(labels); start++ {
		var row strings.Builder
		end := start
		for end < len(labels) && utf8.RuneCountInString(labels[end]) == 1 {
			row.WriteString(strings.ToLower(labels[end]))
			end++
		}
		if end-start >= 5 {
			r := row.String()
			for _, kr := range keyboardRows {
				if strings.Contains(kr, r) || strings.Contains(r, kr[:5]) {
					for i := start; i < end; i++ {
						keys[i] = true
					}
					break
				}
			}
		}
		if end > start {
			start = end - 1
		}
	}
	if len(keys) == 0 {
		return keys
	}
	// The keys between and after the letter rows belong to the keyboard too.
	first, last := len(labels), -1
	for i := range keys {
		if i < first {
			first = i
		}
		if i > last {
			last = i
		}
	}
	for i := first; i <= last; i++ {
		keys[i] = true
	}
	for i := last + 1; i < len(labels); i++ {
		l := strings.ToLower(labels[i])
		if utf8.RuneCountInString(labels[i]) == 1 || keyboardControl[l] {
			keys[i] = true
			continue
		}
		break
	}
	return keys
}

var keyboardControl = map[string]bool{
	"shift": true, "delete": true, "space": true, "return": true, "more": true, "next keyboard": true,
	"dictate": true, "emoji": true, "numbers": true, "search": true, "go": true, "send": true, "done": true,
	"换行": true, "空格": true, "删除": true, "下一个键盘": true, "听写": true, "表情符号": true, "更多": true,
}
