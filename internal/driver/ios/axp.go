package ios

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"howett.net/plist"
)

// This file decodes what the accessibility daemon's remoteAXService sends:
// AccessibilityPlatformTranslation's objects (AXPTranslatorResponse,
// AXPTranslationObject) in an NSKeyedArchiver archive. go-ios's archiver
// only knows Foundation classes, so this is a small reader of its own.

// axpObject is an AXPTranslationObject: one element, named by its process
// and an ID unique within it.
type axpObject struct {
	PID   int
	ID    uint64
	IsApp bool
}

// axpResponse is an AXPTranslatorResponse.
type axpResponse struct {
	RequestType  int
	Attribute    int
	Notification int
	Error        int
	Object       *axpObject // associatedTranslationObject
	Result       any
}

// axpRect is an NSValue holding a CGRect, in points.
type axpRect struct{ X, Y, W, H float64 }

// axpPoint is an NSValue holding a CGPoint, in points.
type axpPoint struct{ X, Y float64 }

// axpRange is an NSValue holding an NSRange.
type axpRange struct{ Loc, Len int }

// axpDict is an NSDictionary; keys keep their decoded type.
type axpDict map[any]any

// keyedArchive resolves the objects of one NSKeyedArchiver archive.
type keyedArchive struct {
	objects []any
	depth   int
}

const maxArchiveDepth = 200

// unarchiveAXP decodes an NSKeyedArchiver archive into Go values: the AXP
// classes above, axpDict, []any, axpRect, string, []byte, and plist scalars.
func unarchiveAXP(data []byte) (any, error) {
	var top struct {
		Archiver string         `plist:"$archiver"`
		Objects  []any          `plist:"$objects"`
		Top      map[string]any `plist:"$top"`
	}
	if _, err := plist.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	if top.Archiver != "NSKeyedArchiver" {
		return nil, fmt.Errorf("not a keyed archive (%q)", top.Archiver)
	}
	root, ok := top.Top["root"]
	if !ok {
		return nil, errors.New("keyed archive without a root")
	}
	a := &keyedArchive{objects: top.Objects}
	return a.resolve(root)
}

func (a *keyedArchive) resolve(v any) (any, error) {
	uid, ok := v.(plist.UID)
	if !ok {
		return v, nil
	}
	if int(uid) >= len(a.objects) {
		return nil, fmt.Errorf("object %d out of range", uid)
	}
	a.depth++
	defer func() { a.depth-- }()
	if a.depth > maxArchiveDepth {
		return nil, errors.New("keyed archive nested too deep")
	}
	o := a.objects[uid]
	if s, ok := o.(string); ok && s == "$null" {
		return nil, nil
	}
	m, ok := o.(map[string]any)
	if !ok {
		return o, nil
	}
	cls := a.className(m["$class"])
	switch cls {
	case "NSDictionary", "NSMutableDictionary":
		keys, _ := m["NS.keys"].([]any)
		vals, _ := m["NS.objects"].([]any)
		if len(keys) != len(vals) {
			return nil, errors.New("dictionary with unequal keys and values")
		}
		out := make(axpDict, len(keys))
		for i := range keys {
			k, err := a.resolve(keys[i])
			if err != nil {
				return nil, err
			}
			v, err := a.resolve(vals[i])
			if err != nil {
				return nil, err
			}
			out[normKey(k)] = v
		}
		return out, nil
	case "NSArray", "NSMutableArray", "NSSet", "NSMutableSet", "NSOrderedSet":
		vals, _ := m["NS.objects"].([]any)
		out := make([]any, len(vals))
		for i := range vals {
			v, err := a.resolve(vals[i])
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case "NSString", "NSMutableString":
		return a.resolve(m["NS.string"])
	case "NSAttributedString", "NSMutableAttributedString":
		return a.resolve(m["NSString"])
	case "NSData", "NSMutableData":
		return a.resolve(m["NS.data"])
	case "NSValue":
		if v, ok := m["NS.rectval"]; ok {
			r, err := a.resolve(v)
			if err != nil {
				return nil, err
			}
			s, _ := r.(string)
			return parseRect(s)
		}
		if v, ok := m["NS.pointval"]; ok {
			r, err := a.resolve(v)
			if err != nil {
				return nil, err
			}
			s, _ := r.(string)
			return parsePoint(s)
		}
		if _, ok := m["NS.rangeval.length"]; ok {
			return axpRange{Loc: toInt(m["NS.rangeval.location"]), Len: toInt(m["NS.rangeval.length"])}, nil
		}
		return m, nil
	case "AXPTranslationObject":
		return &axpObject{PID: toInt(m["pid"]), ID: toUint(m["objectID"], a), IsApp: m["isApplicationElement"] == true}, nil
	case "AXPTranslatorResponse":
		r := &axpResponse{
			RequestType:  toInt(m["associatedRequestType"]),
			Attribute:    toInt(m["attribute"]),
			Notification: toInt(m["notification"]),
			Error:        toInt(m["error"]),
		}
		if o, err := a.resolve(m["associatedTranslationObject"]); err == nil {
			r.Object, _ = o.(*axpObject)
		}
		res, err := a.resolve(m["resultData"])
		if err != nil {
			return nil, err
		}
		r.Result = res
		return r, nil
	}
	// Unknown class: keep its fields, resolved, under their keys.
	out := axpDict{"$class": cls}
	for k, v := range m {
		if k == "$class" {
			continue
		}
		r, err := a.resolve(v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

func (a *keyedArchive) className(v any) string {
	uid, ok := v.(plist.UID)
	if !ok || int(uid) >= len(a.objects) {
		return ""
	}
	m, _ := a.objects[uid].(map[string]any)
	s, _ := m["$classname"].(string)
	return s
}

// normKey makes integer keys comparable whatever their plist width.
func normKey(k any) any {
	switch x := k.(type) {
	case uint64:
		return int(x)
	case int64:
		return int(x)
	}
	return k
}

func toInt(v any) int {
	switch x := v.(type) {
	case uint64:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

// toUint reads an integer that may be stored as its own object.
func toUint(v any, a *keyedArchive) uint64 {
	if uid, ok := v.(plist.UID); ok {
		r, err := a.resolve(uid)
		if err != nil {
			return 0
		}
		v = r
	}
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		return uint64(x)
	}
	return 0
}

// parseRect reads NSStringFromRect's "{{x, y}, {w, h}}".
func parseRect(s string) (axpRect, error) {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == '{' || r == '}' || r == ',' || r == ' ' })
	if len(f) != 4 {
		return axpRect{}, fmt.Errorf("bad rect %q", s)
	}
	var n [4]float64
	for i, p := range f {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return axpRect{}, fmt.Errorf("bad rect %q", s)
		}
		n[i] = v
	}
	return axpRect{n[0], n[1], n[2], n[3]}, nil
}

// axpRequest is an AXPTranslatorRequest.
type axpRequest struct {
	Type      int
	Attribute int
	Object    *axpObject
	Params    map[string]any // values: string, int, []int
	Client    int
}

// archiveAXPRequest encodes a request the way NSKeyedArchiver does, with
// the keys AXPTranslatorRequest's encodeWithCoder: writes.
func archiveAXPRequest(r axpRequest) ([]byte, error) {
	w := &keyedWriter{objects: []any{"$null"}, classes: map[string]plist.UID{}}
	root := map[string]any{}
	rootUID := w.add(root)
	root["requestType"] = uint64(r.Type)
	root["attributeType"] = uint64(r.Attribute)
	root["actionType"] = uint64(0)
	root["clientType"] = uint64(r.Client)
	root["translation"] = plist.UID(0)
	if r.Object != nil {
		root["translation"] = w.add(map[string]any{
			"$class":               w.class("AXPTranslationObject"),
			"bridgeDelegateToken":  plist.UID(0),
			"didPopuldateAppInfo":  false,
			"isApplicationElement": r.Object.IsApp,
			"objectID":             w.add(r.Object.ID),
			"pid":                  uint64(r.Object.PID),
			"rawElementData":       plist.UID(0),
		})
	}
	root["parameters"] = plist.UID(0)
	if r.Params != nil {
		p, err := w.value(r.Params)
		if err != nil {
			return nil, err
		}
		root["parameters"] = p
	}
	root["$class"] = w.class("AXPTranslatorRequest")
	return plist.Marshal(map[string]any{
		"$archiver": "NSKeyedArchiver",
		"$version":  uint64(100000),
		"$top":      map[string]any{"root": rootUID},
		"$objects":  w.objects,
	}, plist.BinaryFormat)
}

type keyedWriter struct {
	objects []any
	classes map[string]plist.UID
}

func (w *keyedWriter) add(v any) plist.UID {
	w.objects = append(w.objects, v)
	return plist.UID(len(w.objects) - 1)
}

func (w *keyedWriter) class(name string) plist.UID {
	if u, ok := w.classes[name]; ok {
		return u
	}
	u := w.add(map[string]any{"$classname": name, "$classes": []any{name, "NSObject"}})
	w.classes[name] = u
	return u
}

func (w *keyedWriter) value(v any) (plist.UID, error) {
	switch x := v.(type) {
	case string:
		return w.add(x), nil
	case int:
		return w.add(int64(x)), nil
	case uint64:
		return w.add(x), nil
	case bool:
		return w.add(x), nil
	case []int:
		items := make([]any, len(x))
		for i, n := range x {
			items[i] = w.add(int64(n))
		}
		return w.add(map[string]any{"$class": w.class("NSArray"), "NS.objects": items}), nil
	case map[string]any:
		keys := make([]any, 0, len(x))
		vals := make([]any, 0, len(x))
		for k, e := range x {
			ev, err := w.value(e)
			if err != nil {
				return 0, err
			}
			keys = append(keys, w.add(k))
			vals = append(vals, ev)
		}
		return w.add(map[string]any{"$class": w.class("NSDictionary"), "NS.keys": keys, "NS.objects": vals}), nil
	}
	return 0, fmt.Errorf("cannot archive %T", v)
}

// parsePoint reads NSStringFromPoint's "{x, y}".
func parsePoint(s string) (axpPoint, error) {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == '{' || r == '}' || r == ',' || r == ' ' })
	if len(f) != 2 {
		return axpPoint{}, fmt.Errorf("bad point %q", s)
	}
	x, err1 := strconv.ParseFloat(f[0], 64)
	y, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil {
		return axpPoint{}, fmt.Errorf("bad point %q", s)
	}
	return axpPoint{x, y}, nil
}
