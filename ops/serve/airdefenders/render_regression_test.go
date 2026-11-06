package main

import (
	"strings"
	"testing"
)

// TestShahedNoseFacesTravel — the drones have been rendered tail-first three
// times in this code base's history ( was the third). Both
// silhouettes are drawn nose-up and the drones fly down the screen, so the
// heading needs the half-turn: atan2(vx, vy) + PI. This pins the expression
// in both pages so the next refactor that "simplifies" it fails here rather
// than on a projector.
func TestShahedNoseFacesTravel(t *testing.T) {
	mp := string(mpHTML)
	if !strings.Contains(mp, "function shahedHeading(vx, vy) { return Math.atan2(vx, vy) + Math.PI; }") {
		t.Fatal("mp.html: shahedHeading must be atan2(vx, vy) + Math.PI (nose-up silhouette, downward travel)")
	}
	if strings.Count(mp, "shahedHeading(") < 3 { // definition + live draw + freeze-frame review
		t.Fatalf("mp.html: expected the live draw and the kill-review to use shahedHeading, found %d uses", strings.Count(mp, "shahedHeading("))
	}
	if strings.Contains(mp, "Math.atan2(vx, vy);") {
		t.Fatal("mp.html: a raw atan2(vx, vy) heading is back — that is the backwards-drone bug")
	}
	sp := string(indexHTML)
	if !strings.Contains(sp, "s.angle = Math.atan2(dx, dy) + Math.PI;") {
		t.Fatal("index.html: the solo shahed heading lost its + Math.PI")
	}
}
