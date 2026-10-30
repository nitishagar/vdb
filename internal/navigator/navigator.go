// Package navigator provides D-pad navigation over the normalized UI tree:
// a pure spatial planner mirroring Vega's Cartesian (geometric-nearest-in-
// direction) focus default, driven by a closed-loop executor — press one
// key, re-read the tree, re-plan — with a hard step budget, trap detection
// (focus cycles), no-progress detection, and virtualized-list scrolling.
package navigator

import (
	"fmt"
	"strings"

	"github.com/nitishagar/vdb/internal/tree"
)

// Selector picks a target element. Grammar (pinned): one clause of
// test_id=<v> | id=<v> | role=<v> | text=<v>.
type Selector struct {
	Field string // "test_id" | "id" | "role" | "text"
	Value string
}

// ParseSelector parses the selector grammar. Unknown prefix errors.
func ParseSelector(s string) (Selector, error) {
	for _, f := range []string{"test_id", "id", "role", "text"} {
		prefix := f + "="
		if strings.HasPrefix(s, prefix) {
			v := strings.TrimPrefix(s, prefix)
			if v == "" {
				return Selector{}, fmt.Errorf("navigator: selector %q has an empty value", s)
			}
			return Selector{Field: f, Value: v}, nil
		}
	}
	return Selector{}, fmt.Errorf("navigator: malformed selector %q (want test_id=…, id=…, role=…, or text=…)", s)
}

// Matches reports whether the element satisfies the selector.
func (s Selector) Matches(e *tree.Element) bool {
	switch s.Field {
	case "test_id":
		return e.TestID == s.Value
	case "id":
		return e.ID == s.Value
	case "role":
		return e.Role == s.Value
	case "text":
		return e.Text == s.Value
	}
	return false
}

// Step is one planner decision.
type Step struct {
	// Key is the direction to press ("UP"|"DOWN"|"LEFT"|"RIGHT"); empty
	// with Done=false means the planner cannot move (same node, no
	// candidates in any direction).
	Key  string
	Done bool
}

// PlanStep chooses the direction to press to move focus from the focused
// element toward the target, mirroring Cartesian focus: the device moves to
// the geometrically nearest candidate in the pressed direction, so pressing
// the dominant-axis direction toward the target is the greedy-optimal move.
func PlanStep(focused, target *tree.Element) Step {
	if focused == nil || target == nil {
		return Step{}
	}
	if sameNode(focused, target) && target.Focused {
		return Step{Done: true}
	}
	fc := center(focused.Rect)
	tc := center(target.Rect)
	dx, dy := tc.x-fc.x, tc.y-fc.y

	// Dominant axis wins; on ties prefer vertical (TV lists read top-down).
	if abs(dy) >= abs(dx) {
		if dy > 0 {
			return Step{Key: "DOWN"}
		}
		if dy < 0 {
			return Step{Key: "UP"}
		}
	}
	if dx > 0 {
		return Step{Key: "RIGHT"}
	}
	if dx < 0 {
		return Step{Key: "LEFT"}
	}
	return Step{}
}

func sameNode(a, b *tree.Element) bool {
	return a.TestID == b.TestID && a.ID == b.ID && a.Role == b.Role && a.Rect == b.Rect
}

type point struct{ x, y int32 }

func center(r tree.Rect) point {
	return point{x: r.X + r.Width/2, y: r.Y + r.Height/2}
}

func abs(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
