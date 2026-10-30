package navigator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nitishagar/vdb/internal/tree"
)

// scriptedSource serves a fixed sequence of page sources, one per read.
type scriptedSource struct {
	pages []string // raw XML
	pos   int
	reads int
}

func (s *scriptedSource) ReadTree(_ context.Context) (*tree.Tree, error) {
	if s.pos >= len(s.pages) {
		return nil, errors.New("scripted source exhausted")
	}
	p := s.pages[s.pos]
	s.pos++
	s.reads++
	return tree.Parse(p)
}

// recordingInjector records every key press.
type recordingInjector struct {
	presses []string
	fail    error
}

func (r *recordingInjector) InjectKey(_ context.Context, _, key string, _ int) error {
	if r.fail != nil {
		return r.fail
	}
	r.presses = append(r.presses, key)
	return nil
}

// gridXML renders a 3x3 grid of focusable cells. Cell (r,c) is at
// x=100+c*400, y=100+r*200, size 300x150 — centers at (250+c*400, 175+r*200).
// focused names the focused cell ("item-rc"); "" focuses nothing.
func gridXML(focused string) string {
	var b strings.Builder
	b.WriteString(`<root><app id="g"><window x="0" y="0" width="1920" height="1080">`)
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			id := "item-" + string(rune('0'+r)) + string(rune('0'+c))
			f := "false"
			if id == focused {
				f = "true"
			}
			b.WriteString(`<child x="` + itoa(100+c*400) + `" y="` + itoa(100+r*200) +
				`" width="300" height="150" role="button" test_id="` + id +
				`" focusable="true" focused="` + f + `"/>`)
		}
	}
	b.WriteString(`</window></app></root>`)
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// Fixture 1 — 3×3 grid, focus at center item-11 (center 650,375), target
// item-22 (center 1050,575).
//
// HAND DERIVATION (geometry, independent of the planner):
//
//	step 1: from item-11 to item-22: dx=+400, dy=+200. |dy|>=|dx| is false
//	        (200<400) → horizontal, dx>0 → RIGHT. Device moves focus to the
//	        geometrically nearest cell to the right: item-12 (same row).
//	step 2: from item-12 (1050,375) to item-22 (1050,575): dx=0, dy=+200 →
//	        DOWN. Device moves to item-22 directly below.
//
// Expected sequence: [RIGHT DOWN].
func TestNavigateGridHandDerived(t *testing.T) {
	src := &scriptedSource{pages: []string{
		gridXML("item-11"), // initial
		gridXML("item-12"), // after RIGHT
		gridXML("item-22"), // after DOWN
	}}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj, Serial: "S"}
	res, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-22"})
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if !res.Reached {
		t.Fatal("must reach target")
	}
	if got, want := strings.Join(res.Steps, ","), "RIGHT,DOWN"; got != want {
		t.Errorf("steps = %s, want %s (hand-derived)", got, want)
	}
	if res.Focus.FocusedID != "test_id:item-22" || res.Focus.TreeDigest == "" {
		t.Errorf("focus state = %+v", res.Focus)
	}
	// Cost model conformance: 1 read per press + 1 initial = 3 reads.
	if src.reads != 3 {
		t.Errorf("tree reads = %d, want 3 (2k+1 with k=2)", src.reads)
	}
}

// Fixture 2 — rail + content. Rail items at y=100..250 (rail-1 x=100,
// rail-2 x=350, rail-3 x=600 — width 200, centers at x+100, y=175); content
// buttons at y=400..550 (content-a x=100, content-b x=600, centers x+200,
// y=475). Focus rail-2, target content-a.
//
// HAND DERIVATION:
//
//	from rail-2 (450,175) to content-a (300,475): dx=-150, dy=+300.
//	|dy|>=|dx| → vertical, dy>0 → DOWN. Device: nearest below rail-2 is
//	content-a (center distance |300-450|=150) not content-b (|800-450|=350).
//
// Expected sequence: [DOWN].
func TestNavigateRailDownToContent(t *testing.T) {
	rail := func(focused string) string {
		var b strings.Builder
		b.WriteString(`<root><app id="r"><window x="0" y="0" width="1920" height="1080">`)
		for i, id := range []string{"rail-1", "rail-2", "rail-3"} {
			f := focused == id
			b.WriteString(`<child x="` + itoa(100+i*250) + `" y="100" width="200" height="150" role="tab" test_id="` + id + `" focusable="true" focused="` + boolStr(f) + `"/>`)
		}
		for _, id := range []string{"content-a", "content-b"} {
			xo := 100
			if id == "content-b" {
				xo = 600
			}
			b.WriteString(`<child x="` + itoa(xo) + `" y="400" width="400" height="150" role="button" test_id="` + id + `" focusable="true" focused="` + boolStr(focused == id) + `"/>`)
		}
		b.WriteString(`</window></app></root>`)
		return b.String()
	}
	src := &scriptedSource{pages: []string{rail("rail-2"), rail("content-a")}}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	res, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "content-a"})
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if got, want := strings.Join(res.Steps, ","), "DOWN"; got != want {
		t.Errorf("steps = %s, want %s (hand-derived)", got, want)
	}
	if !res.Reached {
		t.Error("must reach")
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Fixture 3 — trap: pressing DOWN from A lands on B; pressing DOWN from B
// lands back on A (TVFocusGuideView-style trap). Target C exists below but
// the device never reaches it.
//
// HAND DERIVATION: focus A → plan DOWN → device: B. focus B → plan DOWN
// (C below) → device: A (trap cycles). State (focused=A, unchanged tree) is
// now a REVISIT → ErrTrap after exactly 2 presses.
func TestNavigateTrapTerminates(t *testing.T) {
	node := func(id string, y int, f bool) string {
		return `<child x="100" y="` + itoa(y) + `" width="300" height="100" role="button" test_id="` + id + `" focusable="true" focused="` + boolStr(f) + `"/>`
	}
	trapTree := func(focused string) string {
		var b strings.Builder
		b.WriteString(`<root><app id="t"><window x="0" y="0" width="1920" height="1080">`)
		b.WriteString(node("A", 100, focused == "A"))
		b.WriteString(node("B", 300, focused == "B"))
		b.WriteString(node("C", 500, focused == "C"))
		b.WriteString(`</window></app></root>`)
		return b.String()
	}
	src := &scriptedSource{pages: []string{
		trapTree("A"), // initial
		trapTree("B"), // after first DOWN
		trapTree("A"), // after second DOWN — cycle complete
		trapTree("A"), // (if the loop wrongly continued)
		trapTree("A"),
		trapTree("A"),
	}}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "C"})
	if !errors.Is(err, ErrTrap) {
		t.Fatalf("want ErrTrap, got %v", err)
	}
	if len(inj.presses) != 2 {
		t.Errorf("presses = %v, want exactly [DOWN DOWN] before trap detection", inj.presses)
	}
}

// Fixture 4 — virtualized list: a 3-item window slides down one item per
// DOWN press; target item-4 is outside the initial window (items 1–3).
//
// HAND DERIVATION: target absent → scroll pulse DOWN (window becomes 2–4,
// focus item-2, target now visible). From item-2 plan toward item-4: DOWN
// (device → item-3), then DOWN (device → item-4). Expected [DOWN DOWN DOWN].
func TestNavigateVirtualizedListScrollsIntoView(t *testing.T) {
	window := func(first, focused int) string {
		var b strings.Builder
		b.WriteString(`<root><app id="v"><window x="0" y="0" width="1920" height="1080">`)
		for i := 0; i < 3; i++ {
			id := itoa(first + i)
			b.WriteString(`<child x="100" y="` + itoa(100+i*200) + `" width="500" height="150" role="row" test_id="item-` + id + `" focusable="true" focused="` + boolStr(focused == first+i) + `"/>`)
		}
		b.WriteString(`</window></app></root>`)
		return b.String()
	}
	src := &scriptedSource{pages: []string{
		window(1, 1), // items 1-3, focus item-1, target item-4 absent
		window(2, 2), // after scroll DOWN: items 2-4, focus item-2
		window(2, 3), // after DOWN: focus item-3
		window(2, 4), // after DOWN: focus item-4 — reached
	}}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	res, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-4"})
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if got, want := strings.Join(res.Steps, ","), "DOWN,DOWN,DOWN"; got != want {
		t.Errorf("steps = %s, want %s (hand-derived)", got, want)
	}
}

// Target never appears even after the scroll-pulse bound → structured
// ErrTargetNotFound, ≤ maxScrollPulses presses.
func TestNavigateTargetNotFoundTerminates(t *testing.T) {
	window := func(first, focused int) string {
		var b strings.Builder
		b.WriteString(`<root><app id="v"><window x="0" y="0" width="1920" height="1080">`)
		for i := 0; i < 3; i++ {
			b.WriteString(`<child x="100" y="` + itoa(100+i*200) + `" width="500" height="150" role="row" test_id="item-` + itoa(first+i) + `" focusable="true" focused="` + boolStr(focused == first+i) + `"/>`)
		}
		b.WriteString(`</window></app></root>`)
		return b.String()
	}
	pages := make([]string, 20)
	for i := range pages {
		pages[i] = window(1+i, 1+i)
	}
	src := &scriptedSource{pages: pages}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-999"})
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("want ErrTargetNotFound, got %v", err)
	}
	if len(inj.presses) > maxScrollPulses {
		t.Errorf("presses = %d, want ≤ %d", len(inj.presses), maxScrollPulses)
	}
}

// Infinite corridor: the target is always exactly one row below focus; the
// script regenerates it forever. Budget must stop the loop.
type corridorSource struct{ i int }

func (c *corridorSource) ReadTree(_ context.Context) (*tree.Tree, error) {
	c.i++
	xml := `<root><app id="c"><window x="0" y="0" width="1920" height="1080">` +
		`<child x="100" y="` + itoa(100+c.i*200) + `" width="300" height="100" role="button" test_id="cursor" focusable="true" focused="true"/>` +
		`<child x="100" y="` + itoa(100+(c.i+1)*200) + `" width="300" height="100" role="button" test_id="end" focusable="true" focused="false"/>` +
		`</window></app></root>`
	return tree.Parse(xml)
}

func TestNavigateBudgetExceeded(t *testing.T) {
	inj := &recordingInjector{}
	x := &Executor{Source: &corridorSource{}, Keys: inj, Budget: 5}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "end"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
	if len(inj.presses) != 5 {
		t.Errorf("presses = %d, want exactly budget 5", len(inj.presses))
	}
}

// Already focused → zero presses, reached immediately.
func TestNavigateAlreadyFocused(t *testing.T) {
	src := &scriptedSource{pages: []string{gridXML("item-00")}}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	res, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-00"})
	if err != nil || !res.Reached {
		t.Fatalf("err=%v reached=%v", err, res.Reached)
	}
	if len(res.Steps) != 0 || len(inj.presses) != 0 {
		t.Errorf("steps = %v, want none", res.Steps)
	}
}

// Frozen tree WITH the target present: the same focus+tree state is
// revisited → the trap detector fires (a focus cycle is exactly this
// signature). Structured ErrTrap, no infinite loop.
func TestNavigateFrozenTreeWithTargetIsTrap(t *testing.T) {
	pages := make([]string, 10)
	for i := range pages {
		pages[i] = gridXML("item-11")
	}
	src := &scriptedSource{pages: pages}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-22"})
	if !errors.Is(err, ErrTrap) {
		t.Fatalf("want ErrTrap (revisited focus+tree state), got %v", err)
	}
}

// Frozen tree WITHOUT the target: scroll pulses can't move anything → two
// stasis strikes → ErrNoProgress (the alternate-axis fallback is unavailable
// because the target is absent).
func TestNavigateNoProgressTerminates(t *testing.T) {
	frozen := `<root><app id="s"><window x="0" y="0" width="1920" height="1080">` +
		`<child x="100" y="100" width="300" height="100" role="row" test_id="only" focusable="true" focused="true"/>` +
		`</window></app></root>`
	pages := make([]string, 10)
	for i := range pages {
		pages[i] = frozen
	}
	src := &scriptedSource{pages: pages}
	inj := &recordingInjector{}
	x := &Executor{Source: src, Keys: inj}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "missing"})
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("want ErrNoProgress, got %v", err)
	}
	if len(inj.presses) != 2 {
		t.Errorf("presses = %v, want two stasis strikes", inj.presses)
	}
}

// Inject failures surface as structured errors (no swallowed failures).
func TestNavigateInjectorFailurePropagates(t *testing.T) {
	src := &scriptedSource{pages: []string{gridXML("item-11")}}
	inj := &recordingInjector{fail: errors.New("device gone")}
	x := &Executor{Source: src, Keys: inj}
	_, err := x.Navigate(context.Background(), Selector{Field: "test_id", Value: "item-22"})
	if err == nil || !strings.Contains(err.Error(), "device gone") {
		t.Fatalf("want propagated inject error, got %v", err)
	}
}

func TestParseSelectorGrammar(t *testing.T) {
	for _, ok := range []string{"test_id=x", "id=y", "role=button", "text=Hi There"} {
		if _, err := ParseSelector(ok); err != nil {
			t.Errorf("ParseSelector(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "color=red", "test_id=", "=x", "test_id"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("ParseSelector(%q) must error", bad)
		}
	}
}

func TestPlanStepTiePrefersVertical(t *testing.T) {
	// dx=+200, dy=+200 (tie) → vertical wins → DOWN.
	f := &tree.Element{TestID: "f", Rect: tree.Rect{X: 0, Y: 0, Width: 100, Height: 100}}
	g := &tree.Element{TestID: "g", Rect: tree.Rect{X: 200, Y: 200, Width: 100, Height: 100}}
	if got := PlanStep(f, g).Key; got != "DOWN" {
		t.Errorf("tie key = %s, want DOWN", got)
	}
	// Pure left.
	g.Rect = tree.Rect{X: -300, Y: 0, Width: 100, Height: 100}
	if got := PlanStep(f, g).Key; got != "LEFT" {
		t.Errorf("key = %s, want LEFT", got)
	}
	// Overlapping centers (no direction) → no key, not done.
	g.Rect = f.Rect
	if s := PlanStep(f, g); s.Key != "" || s.Done {
		t.Errorf("zero-delta plan = %+v", s)
	}
}
