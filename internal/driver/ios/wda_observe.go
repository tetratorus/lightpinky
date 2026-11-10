package ios

import (
	"fmt"
	"math"
	"strings"

	"github.com/tetratorus/lightpinky/internal/screen"
)

// xcTypes maps XCUIElementType names, as WDA writes them, to their numbers.
var xcTypes = map[string]int{
	"Any": 0, "Other": 1, "Application": 2, "Group": 3, "Window": 4, "Sheet": 5,
	"Drawer": 6, "Alert": 7, "Dialog": 8, "Button": 9, "RadioButton": 10,
	"RadioGroup": 11, "CheckBox": 12, "DisclosureTriangle": 13, "PopUpButton": 14,
	"ComboBox": 15, "MenuButton": 16, "ToolbarButton": 17, "Popover": 18,
	"Keyboard": 19, "Key": 20, "NavigationBar": 21, "TabBar": 22, "TabGroup": 23,
	"Toolbar": 24, "StatusBar": 25, "Table": 26, "TableRow": 27, "TableColumn": 28,
	"Outline": 29, "OutlineRow": 30, "Browser": 31, "CollectionView": 32,
	"Slider": 33, "PageIndicator": 34, "ProgressIndicator": 35,
	"ActivityIndicator": 36, "SegmentedControl": 37, "Picker": 38,
	"PickerWheel": 39, "Switch": 40, "Toggle": 41, "Link": 42, "Image": 43,
	"Icon": 44, "SearchField": 45, "ScrollView": 46, "ScrollBar": 47,
	"StaticText": 48, "TextField": 49, "SecureTextField": 50, "DatePicker": 51,
	"TextView": 52, "Menu": 53, "MenuItem": 54, "MenuBar": 55, "MenuBarItem": 56,
	"Map": 57, "WebView": 58, "IncrementArrow": 59, "DecrementArrow": 60,
	"Timeline": 61, "RatingIndicator": 62, "ValueIndicator": 63,
	"SplitGroup": 64, "Splitter": 65, "RelevanceIndicator": 66, "ColorWell": 67,
	"HelpTag": 68, "Matte": 69, "DockItem": 70, "Ruler": 71, "RulerMarker": 72,
	"Grid": 73, "LevelIndicator": 74, "Cell": 75, "LayoutArea": 76,
	"LayoutItem": 77, "Handle": 78, "Stepper": 79, "Tab": 80, "TouchBar": 81,
	"StatusItem": 82,
}

const xcStatusBar = 25

// wdaTraitBits maps the trait names in WDA's "traits" string to
// UIAccessibilityTraits bits.
var wdaTraitBits = map[string]uint64{
	"Button":      traitButton,
	"Link":        traitLink,
	"Image":       traitImage,
	"Selected":    traitSelected,
	"KeyboardKey": traitKeyboardKey,
	"StaticText":  traitStaticText,
	"NotEnabled":  traitNotEnabled,
	"SearchField": traitSearchField,
	"TabBar":      traitTabBar,
	"Header":      traitHeader,
}

func wdaTraits(s string) uint64 {
	var bits uint64
	for _, name := range strings.Split(s, ",") {
		bits |= wdaTraitBits[strings.TrimSpace(name)]
	}
	return bits
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// wdaValue turns WDA's value, a string or a number, into text.
func wdaValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(x)
	}
}

// raxFromWDA describes a WDA node the way the remoteAXService path does, so
// both paths share their roles and labels.
func raxFromWDA(n *wdaNode) raxElement {
	e := raxElement{
		Label:       strings.TrimSpace(strOr(n.Label)),
		Value:       wdaValue(n.Value),
		Placeholder: strOr(n.Placeholder),
		Ident:       strOr(n.Identifier),
		Frame:       axpRect{X: n.Rect.X, Y: n.Rect.Y, W: n.Rect.Width, H: n.Rect.Height},
		HasFrame:    n.Rect.Width > 0 && n.Rect.Height > 0,
		Type:        xcTypes[n.Type],
		Traits:      wdaTraits(n.Traits),
		Enabled:     n.Enabled == nil || bool(*n.Enabled),
		Focused:     bool(n.Focused),
		Selected:    bool(n.Selected),
		Visible:     bool(n.Visible),
		Children:    len(n.Children),
	}
	// WDA repeats the label as the value of static texts.
	if e.Type == xcStaticText && e.Value == e.Label {
		e.Value = ""
	}
	// An identifier that only repeats the label says nothing.
	if e.Ident == e.Label {
		e.Ident = ""
	}
	return e
}

// wdaIsElement reports whether an agent sees the node: an accessibility
// element on screen, or a text field or switch that its app forgot to mark
// as one. Containers such as cells are left out: the accessibility element
// inside carries their label (a Settings row is a Cell holding a Button
// labelled with the row's title).
func wdaIsElement(accessible bool, e raxElement) bool {
	if !e.Visible || !e.HasFrame {
		return false
	}
	if accessible {
		return true
	}
	switch e.Type {
	case xcTextField, xcSecureTextField, xcSearchField, xcTextView, xcSwitch, xcToggle:
		return true
	case xcButton, xcLink:
		return e.Label != ""
	}
	return false
}

// XCUIElementType numbers of the bars content scrolls under.
const (
	xcNavigationBar = 21
	xcToolbar       = 24
)

// wdaScreenElements flattens WDA's page source into Pinky's elements, in
// pixels, in the tree's order (which is reading order). The keyboard's keys
// only mark the keyboard as shown, and the status bar is left out, as on the
// iOS 27 path.
//
// Two things WDA lists that a person does not see as separate (GitHub
// issue #7, WeChat's chat list): a text inside an element whose label
// already says it (a cell that is one accessibility element, whose labels
// XCTest still lists), and elements scrolled under the navigation bar, a
// tab bar, a toolbar, the status bar or the keyboard, which WDA counts as
// visible. The texts are left out; covered elements too, unless a good part
// of them shows, as on the iOS 27 path (unoccluded).
func wdaScreenElements(root *wdaNode, scale float64, w, h int, appID string) ([]screen.Element, bool) {
	var out []screen.Element
	var over []occluder
	keyboard := false
	var walk func(n *wdaNode, path string, inKeyboard, inTabBar bool, said string, depth int)
	walk = func(n *wdaNode, path string, inKeyboard, inTabBar bool, said string, depth int) {
		if depth > 80 {
			return
		}
		e := raxFromWDA(n)
		child := path
		if depth > 0 {
			child = path + "/" + xcName(e)
		}
		bar := screen.Rect{}
		if e.Visible && e.HasFrame && depth > 0 {
			bar = pixelRect(e.Frame, scale, w, h)
		}
		switch e.Type {
		case xcStatusBar:
			if !bar.Empty() && bar.H <= h/8 {
				over = append(over, occluder{r: bar})
			}
			return
		case xcKeyboard:
			inKeyboard = true
			if e.Visible {
				keyboard = true
				if !bar.Empty() {
					over = append(over, occluder{r: bar})
				}
			}
		case xcTabBar:
			inTabBar = true
		}
		if isBar(e.Type, bar, h) {
			barPath, r := child, bar
			over = append(over, occluder{r: bar, member: func(el screen.Element) bool {
				return strings.HasPrefix(el.Path, barPath) || (el.Path == path && el.Bounds == r)
			}})
		}
		if depth > 0 && !inKeyboard && wdaIsElement(bool(n.Accessible), e) {
			if r := pixelRect(e.Frame, scale, w, h); !r.Empty() {
				role, states := raxRole(e, inTabBar)
				label := raxLabel(e, role)
				switch {
				case role == screen.RoleText && said != "" && norm(label) != "" && strings.Contains(said, norm(label)):
					// Its element says it already.
				case label != "" || role != screen.RoleOther:
					out = append(out, screen.Element{
						Role: role, Label: label, Bounds: r, States: states,
						Source: screen.SourceTree, Path: path, ID: e.Ident,
					})
					if label != "" {
						said = norm(label)
					}
				}
			}
		}
		for i := range n.Children {
			walk(&n.Children[i], child, inKeyboard, inTabBar, said, depth+1)
		}
	}
	walk(root, appID, false, false, "", 0)
	return unoccluded(out, over, int(math.Round(minVisiblePoints*scale))), keyboard
}

// isBar reports whether a node is a bar that content scrolls under: a
// navigation bar along the top, or a tab bar or toolbar along the bottom,
// no taller than a quarter of the screen.
func isBar(typ int, r screen.Rect, h int) bool {
	if r.Empty() || r.H > h/4 {
		return false
	}
	switch typ {
	case xcNavigationBar:
		return r.Y < h/4
	case xcTabBar, xcToolbar:
		return r.Y+r.H > h*3/4
	}
	return false
}
