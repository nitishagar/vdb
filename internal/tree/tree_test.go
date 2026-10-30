package tree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func mustParse(t *testing.T, xml string) *Tree {
	t.Helper()
	tr, err := Parse(xml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return tr
}

func TestParseSimpleAppGeometryAndFlags(t *testing.T) {
	tr := mustParse(t, fixture(t, "simple_app.xml"))
	hero := tr.Find(func(e *Element) bool { return e.TestID == "hero-button" })
	if hero == nil {
		t.Fatal("hero-button not found")
	}
	if hero.Rect.X != 100 || hero.Rect.Y != 100 || hero.Rect.Width != 400 || hero.Rect.Height != 80 {
		t.Errorf("hero rect = %+v", hero.Rect)
	}
	if !hero.Focusable || hero.Focused {
		t.Errorf("hero flags: focusable=%v focused=%v", hero.Focusable, hero.Focused)
	}
	if hero.AppID != "com.example.app" {
		t.Errorf("app id = %q", hero.AppID)
	}
	list := tr.Find(func(e *Element) bool { return e.TestID == "list-button" })
	if list == nil || !list.Focused {
		t.Fatal("list-button must be the focused element")
	}
	if tr.Focused() == nil || tr.Focused().TestID != "list-button" {
		t.Errorf("Focused() = %+v", tr.Focused())
	}
	// Zero-size image node is preserved, not dropped (boundary input).
	img := tr.Find(func(e *Element) bool { return e.ID == "poster-1" })
	if img == nil {
		t.Fatal("zero-geometry node must be preserved")
	}
}

func TestParseMultiAppAndFilter(t *testing.T) {
	tr := mustParse(t, fixture(t, "multi_app.xml"))
	if got := len(tr.Focusables()); got != 2 {
		t.Errorf("focusables across apps = %d, want 2 (resume + episodes; launcher menu is not focusable)", got)
	}
	app := tr.Filter("com.example.app")
	if app.Find(func(e *Element) bool { return e.TestID == "launcher-tiles" }) != nil {
		t.Error("filtered tree must not contain the launcher")
	}
	if app.Find(func(e *Element) bool { return e.TestID == "episodes" }) == nil {
		t.Error("filtered tree lost the app's own nodes")
	}
}

func TestFocusAlignmentDefectSurfaced(t *testing.T) {
	tr := mustParse(t, fixture(t, "focus_defect.xml"))
	found := false
	for _, w := range tr.Warnings {
		if strings.Contains(w, "focus hierarchy") {
			found = true
		}
	}
	if !found {
		t.Errorf("multiple focused nodes must surface the defect warning; warnings=%v", tr.Warnings)
	}
	// And it must NOT be "fixed": both focused flags survive verbatim.
	n := countFocused(&tr.Root)
	if n != 2 {
		t.Errorf("focused count = %d, want 2 (never silently corrected)", n)
	}
}

func TestHashStabilityAndSensitivity(t *testing.T) {
	a := mustParse(t, fixture(t, "simple_app.xml"))
	b := mustParse(t, fixture(t, "simple_app.xml"))
	if a.Hash() != b.Hash() {
		t.Error("identical trees must hash identically")
	}
	// Flip one focused flag: hash must change (wait_idle depends on it).
	var flip func(*Element)
	flip = func(e *Element) {
		if e.TestID == "hero-button" {
			e.Focused = true
		}
		for i := range e.Children {
			flip(&e.Children[i])
		}
	}
	flip(&b.Root)
	if a.Hash() == b.Hash() {
		t.Error("focus flip must change the tree hash")
	}
}

func TestGoldenJSONRoundTrip(t *testing.T) {
	for _, name := range []string{"simple_app.xml", "multi_app.xml", "focus_defect.xml"} {
		tr := mustParse(t, fixture(t, name))
		tr.SortWarnings()
		jsonOut, err := tr.MarshalJSONString()
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		goldenPath := filepath.Join("testdata", strings.TrimSuffix(name, ".xml")+".golden.json")
		if _, err := os.Stat(goldenPath); os.IsNotExist(err) {
			// First run writes the golden (regenerate with -update).
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				os.WriteFile(goldenPath, []byte(jsonOut), 0o644)
				continue
			}
			t.Fatalf("golden %s missing (run UPDATE_GOLDEN=1 go test ./internal/tree/ once)", goldenPath)
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != jsonOut {
			t.Errorf("%s: normalized JSON drifted from golden", name)
		}
		// Round-trip law: FromJSON(marshal) re-marshals identically.
		back, err := FromJSON(jsonOut)
		if err != nil {
			t.Fatalf("%s: from json: %v", name, err)
		}
		back.SortWarnings()
		again, _ := back.MarshalJSONString()
		if again != jsonOut {
			t.Errorf("%s: JSON round-trip not idempotent", name)
		}
	}
}

func TestParseMalformedXMLError(t *testing.T) {
	if _, err := Parse("<root><unclosed>"); err == nil {
		t.Error("malformed XML must error")
	}
}

func TestParseMissingAttributesZeroValuesAndWarnings(t *testing.T) {
	tr := mustParse(t, `<root><app id="a"><window><child role="row"/></window></app></root>`)
	row := tr.Find(func(e *Element) bool { return e.Role == "row" })
	if row == nil {
		t.Fatal("row missing")
	}
	if row.Rect != (Rect{}) {
		t.Errorf("rect = %+v, want zero", row.Rect)
	}
}

func TestParseLargeTreePerformance(t *testing.T) {
	// 1000-node generated tree parses in < 1s (envelope boundary).
	var sb strings.Builder
	sb.WriteString("<root><app id=\"gen\"><window x=\"0\" y=\"0\" width=\"1920\" height=\"1080\">")
	for i := 0; i < 1000; i++ {
		sb.WriteString(`<child x="0" y="0" width="10" height="10" role="row" focusable="true"/>`)
	}
	sb.WriteString("</window></app></root>")
	start := time.Now()
	tr := mustParse(t, sb.String())
	if d := time.Since(start); d > time.Second {
		t.Errorf("1000-node parse took %v", d)
	}
	if got := len(tr.Focusables()); got != 1000 {
		t.Errorf("focusables = %d", got)
	}
}

func TestSummaryBoundsAndTruncation(t *testing.T) {
	tr := mustParse(t, fixture(t, "multi_app.xml"))
	s := tr.Summary(3)
	if len(s.Nodes) != 3 {
		t.Errorf("nodes = %d, want cap 3", len(s.Nodes))
	}
	if !s.Truncated {
		t.Error("truncation must be marked")
	}
	if !strings.Contains(s.String(), "truncated at 3") {
		t.Errorf("rendered summary missing truncation marker:\n%s", s.String())
	}
	full := tr.Summary(1000)
	if full.Truncated {
		t.Error("large cap must not truncate")
	}
	if !strings.Contains(full.String(), "[*]") {
		t.Error("focused marker missing in outline")
	}
}

func TestOneNodeTree(t *testing.T) {
	tr := mustParse(t, "<root/>")
	if tr.Focused() != nil {
		t.Error("empty tree has no focus")
	}
	if tr.Hash() == "" {
		t.Error("hash must exist")
	}
}

// I3 regression: geometry never silently corrupts — overflow and malformed
// values become zero WITH a parse warning (invariant 7).
func TestParseGeometryOverflowAndMalformedWarn(t *testing.T) {
	tr := mustParse(t, `<root><app id="o"><window x="0" y="0" width="1920" height="1080">`+
		`<child x="2147483648" y="-2147483649" width="12ab" height="50" role="cell" test_id="bad" focusable="true"/>`+
		`</window></app></root>`)
	cell := tr.Find(func(e *Element) bool { return e.TestID == "bad" })
	if cell == nil {
		t.Fatal("cell missing")
	}
	if cell.Rect.X != 0 || cell.Rect.Y != 0 || cell.Rect.Width != 0 {
		t.Errorf("corrupt geometry must be zero, got %+v", cell.Rect)
	}
	if cell.Rect.Height != 50 {
		t.Errorf("valid field must parse, got %+v", cell.Rect)
	}
	if len(tr.Warnings) < 3 {
		t.Errorf("expected ≥3 parse warnings, got %v", tr.Warnings)
	}
}

func TestParseMissingAttributesAssertsWarning(t *testing.T) {
	tr := mustParse(t, `<root><app id="a"><window><child role="row"/></window></app></root>`)
	if len(tr.Warnings) == 0 {
		t.Fatal("a leaf node with no geometry attributes must produce a parse warning")
	}
	found := false
	for _, w := range tr.Warnings {
		if strings.Contains(w, "no geometry") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", tr.Warnings)
	}
}
