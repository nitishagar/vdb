package navigator

import (
	"context"
	"errors"
	"fmt"

	"github.com/nitishagar/vdb/internal/tree"
)

// Errors: structured termination reasons — never an infinite press loop
// (invariant 8).
var (
	ErrBudgetExceeded = errors.New("navigator: step budget exceeded before reaching target")
	ErrNoProgress     = errors.New("navigator: focus stopped progressing toward target")
	ErrTrap           = errors.New("navigator: focus cycle detected (trap)")
	ErrTargetNotFound = errors.New("navigator: target never appeared in the tree after scrolling")
)

// FocusState is the post-action focus snapshot every navigation result
// carries (invariant 20) so agent loops close in one round-trip.
type FocusState struct {
	FocusedID  string `json:"focused_id"`
	TreeDigest string `json:"tree_digest"`
}

// Result is a completed navigation.
type Result struct {
	Steps   []string   // keys pressed, in order
	Focus   FocusState // final focus
	Reached bool
}

// TreeSource reads the current page source (toolkit-backed in production).
type TreeSource interface {
	ReadTree(ctx context.Context) (*tree.Tree, error)
}

// KeyInjector presses one key (input.Backend satisfies this).
type KeyInjector interface {
	InjectKey(ctx context.Context, serial, key string, holdMS int) error
}

// Executor runs closed-loop navigation: exactly one tree read per press
// (the post-press read doubles as the next planning state), matching the
// plan's cost model of 2k+1 device calls for k presses. Budget is the hard
// bound on TOTAL presses — plan-path, scroll, and alternate-axis alike.
type Executor struct {
	Source TreeSource
	Keys   KeyInjector
	Serial string
	Budget int // max presses; default 16 when zero
}

// Navigate moves focus to the element matching sel, re-reading the tree
// after every press. The loop always terminates within Budget TOTAL presses
// or a structured error.
func (x *Executor) Navigate(ctx context.Context, sel Selector) (Result, error) {
	budget := x.Budget
	if budget <= 0 {
		budget = 16
	}
	res := Result{Steps: []string{}}

	tr, err := x.Source.ReadTree(ctx)
	if err != nil {
		return res, fmt.Errorf("navigator: read tree: %w", err)
	}

	// visited detects traps: same focused node + same tree hash seen before.
	visited := map[string]bool{}
	noProgress := 0
	scrollPulses := 0

	for {
		target := tr.Find(sel.Matches)

		if target == nil {
			// Virtualized list heuristic: the target is below the rendered
			// window in the common case — bounded scroll pulses, re-check.
			if scrollPulses >= maxScrollPulses {
				return res, x.fail(tr, fmt.Errorf("%w (selector %s=%s)", ErrTargetNotFound, sel.Field, sel.Value))
			}
			scrollPulses++
			if err := x.advance(ctx, &res, tr, "DOWN", budget, &noProgress, sel); err != nil {
				return res, x.fail(tr, err)
			}
			continue
		}

		focused := tr.Focused()
		if focused != nil && sameNode(focused, target) {
			res.Reached = true
			res.Focus = FocusState{FocusedID: idOf(focused), TreeDigest: tr.Hash()}
			return res, nil
		}

		step := PlanStep(focused, target)
		if step.Key == "" {
			return res, x.fail(tr, fmt.Errorf("%w (planner cannot move toward %s=%s)", ErrNoProgress, sel.Field, sel.Value))
		}

		// Trap check BEFORE pressing: revisit of an exact focus+tree state.
		stateKey := fmt.Sprintf("%s|%s", idOf(focused), tr.Hash())
		if visited[stateKey] {
			return res, x.fail(tr, fmt.Errorf("%w at %s after %d steps", ErrTrap, idOf(focused), len(res.Steps)))
		}
		visited[stateKey] = true

		if err := x.advance(ctx, &res, tr, step.Key, budget, &noProgress, sel); err != nil {
			return res, x.fail(tr, err)
		}
		// advance updated tr to the post-press tree.
	}
}

const maxScrollPulses = 4

// advance presses key, reads the post-press tree into *cur, and updates
// no-progress tracking. The post-press read doubles as the next planning
// state (one read per press). The budget check lives HERE so every press
// kind (plan, scroll, alternate axis) is bounded.
func (x *Executor) advance(ctx context.Context, res *Result, cur *tree.Tree, key string, budget int, noProgress *int, sel Selector) error {
	if len(res.Steps) >= budget {
		return fmt.Errorf("%w (%d steps, budget %d)", ErrBudgetExceeded, len(res.Steps), budget)
	}
	preFocused := cur.Focused()
	if err := x.Keys.InjectKey(ctx, x.Serial, key, 0); err != nil {
		return fmt.Errorf("navigator: inject %s: %w", key, err)
	}
	res.Steps = append(res.Steps, key)

	next, err := x.Source.ReadTree(ctx)
	if err != nil {
		return fmt.Errorf("navigator: verify tree: %w", err)
	}

	// No-progress: focus unchanged AND tree unchanged after a press.
	nextFocused := next.Focused()
	moved := nextFocused == nil || preFocused == nil || !sameNode(nextFocused, preFocused)
	if !moved && next.Hash() == cur.Hash() {
		*noProgress++
		if *noProgress >= 2 {
			return fmt.Errorf("%w: pressing %s left focus on %s", ErrNoProgress, key, idOf(preFocused))
		}
		// One alternate-axis press before declaring stasis.
		if alt := alternateAxis(key, preFocused, next.Find(sel.Matches)); alt != "" && len(res.Steps) < budget {
			if err := x.Keys.InjectKey(ctx, x.Serial, alt, 0); err != nil {
				return fmt.Errorf("navigator: inject %s: %w", alt, err)
			}
			res.Steps = append(res.Steps, alt)
			after, err := x.Source.ReadTree(ctx)
			if err != nil {
				return fmt.Errorf("navigator: verify alt tree: %w", err)
			}
			*cur = *after
			return nil
		}
	} else {
		*noProgress = 0
	}
	*cur = *next
	return nil
}

// fail records the last-known focus state on a failing result (diagnostics
// for agents — invariant 20) and returns the error unchanged.
func (x *Executor) fail(tr *tree.Tree, err error) error {
	if f := tr.Focused(); f != nil {
		return focusError{err, FocusState{FocusedID: idOf(f), TreeDigest: tr.Hash()}}
	}
	return err
}

// focusError decorates a termination error with the last-observed focus.
type focusError struct {
	cause error
	state FocusState
}

func (e focusError) Error() string { return e.cause.Error() }
func (e focusError) Unwrap() error { return e.cause }

// FocusDiagnostics extracts the last-known focus state from a navigation
// error, if present.
func FocusDiagnostics(err error) (FocusState, bool) {
	var fe focusError
	if errors.As(err, &fe) {
		return fe.state, true
	}
	return FocusState{}, false
}

func alternateAxis(last string, focused, target *tree.Element) string {
	if focused == nil || target == nil {
		return ""
	}
	fc := center(focused.Rect)
	tc := center(target.Rect)
	dx, dy := tc.x-fc.x, tc.y-fc.y
	switch last {
	case "UP", "DOWN":
		if dx > 0 {
			return "RIGHT"
		}
		if dx < 0 {
			return "LEFT"
		}
	case "LEFT", "RIGHT":
		if dy > 0 {
			return "DOWN"
		}
		if dy < 0 {
			return "UP"
		}
	}
	return ""
}

func idOf(e *tree.Element) string {
	if e == nil {
		return ""
	}
	if e.TestID != "" {
		return "test_id:" + e.TestID
	}
	if e.ID != "" {
		return "id:" + e.ID
	}
	return fmt.Sprintf("%s@%d,%d", e.Role, e.Rect.X, e.Rect.Y)
}
