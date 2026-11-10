package ios

import "testing"

func TestInferVisibleClipsToAncestors(t *testing.T) {
	root := wdaNode{Rect: wdaRect{Width: 402, Height: 874}, Children: []wdaNode{
		{Type: "Table", Rect: wdaRect{Y: 100, Width: 402, Height: 600}, Children: []wdaNode{
			{Type: "Cell", Rect: wdaRect{Y: 650, Width: 402, Height: 52}},  // partly inside the table
			{Type: "Cell", Rect: wdaRect{Y: 720, Width: 402, Height: 52}},  // on screen, scrolled out of the table
			{Type: "Cell", Rect: wdaRect{Y: 1200, Width: 402, Height: 52}}, // off screen
		}},
		{Type: "Other", Rect: wdaRect{X: 10, Y: 10}}, // no frame
	}}
	clip := root.Rect
	clip.Y, clip.Height = 54, clip.Height-54
	inferVisible(&root, clip)
	table := root.Children[0]
	for i, want := range []bool{true, false, false} {
		if got := bool(table.Children[i].Visible); got != want {
			t.Errorf("cell %d visible %v, want %v", i, got, want)
		}
	}
	if !bool(table.Visible) || bool(root.Children[1].Visible) {
		t.Errorf("table %v, frameless %v", table.Visible, root.Children[1].Visible)
	}
}
