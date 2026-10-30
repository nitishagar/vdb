// Package tree normalizes the automation toolkit's page-source XML into a
// typed element model, hashes trees for stability detection (wait_idle),
// filters multi-app trees to a foreground app, and produces bounded compact
// summaries for CLI/MCP output.
//
// Schema (Amazon-observed, doc:94/103): root/app/window/child nesting;
// attributes x, y, width, height, id, test_id, role, focusable, focused;
// <traits> children carrying visibility, enableable, focusable setFocus/
// focused. Trees span several apps including the launcher.
package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Element is one normalized UI node. Geometry and flags round-trip from the
// toolkit XML without loss (invariant 7).
type Element struct {
	ID        string    `json:"id,omitempty"`
	TestID    string    `json:"test_id,omitempty"`
	Role      string    `json:"role,omitempty"`
	Text      string    `json:"text,omitempty"`
	AppID     string    `json:"app_id,omitempty"`
	Rect      Rect      `json:"rect"`
	Focusable bool      `json:"focusable"`
	Focused   bool      `json:"focused"`
	Enabled   bool      `json:"enabled"`
	Visible   bool      `json:"visible"`
	Children  []Element `json:"children,omitempty"`
}

// Rect is integer screen geometry.
type Rect struct {
	X      int32 `json:"x"`
	Y      int32 `json:"y"`
	Width  int32 `json:"w"`
	Height int32 `json:"h"`
}

// Tree is a normalized page source.
type Tree struct {
	Root     Element  `json:"root"`
	Warnings []string `json:"warnings,omitempty"`
}

// Warning marks an observed toolkit quirk surfaced to callers — never
// silently corrected (invariant 7): the documented focus-alignment defect.
func FocusAlignmentWarning() string {
	return "toolkit focus hierarchy may not align with actual focused elements (known toolkit defect)"
}

// xmlNode is the intermediate parsed node the normalizer consumes.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Traits   []xmlTrait `xml:"traits"`
	Children []xmlNode  `xml:",any"`
}

// xmlTrait carries one <trait> element's attributes.
type xmlTrait struct {
	Attrs []xml.Attr `xml:",any,attr"`
}

func countFocused(e *Element) int {
	n := 0
	if e.Focused {
		n++
	}
	for i := range e.Children {
		n += countFocused(&e.Children[i])
	}
	return n
}

// maxDepth bounds XML nesting: a hostile or corrupted page source must not
// exhaust the goroutine stack, so parsing is token-based with an explicit
// element stack (no recursion, depth-bounded).
const maxDepth = 4096

// Parse decodes toolkit page-source XML. Missing attributes become
// zero-values with a warning appended; zero-size nodes are preserved
// (virtualized/offscreen content), not dropped.
func Parse(pageSourceXML string) (*Tree, error) {
	raw, err := parseTokens(pageSourceXML)
	if err != nil {
		return nil, err
	}
	t := &Tree{}
	t.Root = raw.toElement(nil, t)
	// Black-box signal of the documented focus-alignment defect: more than
	// one node claiming focus. Surfaced, never corrected (invariant 7).
	if n := countFocused(&t.Root); n > 1 {
		t.Warnings = append(t.Warnings, FocusAlignmentWarning()+fmt.Sprintf(" (%d nodes claim focus)", n))
	}
	return t, nil
}

// bnode is the builder node: children are pointers so later appends never
// invalidate earlier references.
type bnode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     strings.Builder
	traits   []xml.Attr // flattened attrs of <trait> children
	children []*bnode
}

// parseTokens builds the tree with an explicit stack. <traits> elements do
// not become nodes: their <trait> children's attributes attach to the
// owning element (matching the toolkit schema).
func parseTokens(src string) (*xmlNode, error) {
	dec := xml.NewDecoder(strings.NewReader(src))
	dec.Strict = false
	var root *bnode
	var stack []*bnode
	var traitsOwner *bnode

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tree: malformed page source: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) >= maxDepth {
				return nil, fmt.Errorf("tree: page source exceeds %d nesting levels", maxDepth)
			}
			switch {
			case t.Name.Local == "traits" && len(stack) > 0:
				traitsOwner = stack[len(stack)-1]
			case t.Name.Local == "trait" && traitsOwner != nil:
				traitsOwner.traits = append(traitsOwner.traits, t.Attr...)
			default:
				n := &bnode{name: t.Name, attrs: t.Attr}
				if root == nil {
					root = n
				} else if len(stack) > 0 {
					parent := stack[len(stack)-1]
					parent.children = append(parent.children, n)
				}
				stack = append(stack, n)
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "traits":
				traitsOwner = nil
			case "trait":
				// trait never pushed the stack
			default:
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("tree: empty page source")
	}
	out := buildXMLNode(root)
	return &out, nil
}

// buildXMLNode converts the pointer builder tree to the value xmlNode tree
// (recursion bounded by maxDepth).
func buildXMLNode(b *bnode) xmlNode {
	n := xmlNode{XMLName: b.name, Attrs: b.attrs, Text: b.text.String()}
	for _, tr := range b.traits {
		n.Traits = append(n.Traits, xmlTrait{Attrs: []xml.Attr{tr}})
	}
	for _, c := range b.children {
		n.Children = append(n.Children, buildXMLNode(c))
	}
	return n
}

func (n *xmlNode) attr(name string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value, true
		}
	}
	return "", false
}

func (n *xmlNode) toElement(appID *string, t *Tree) Element {
	e := Element{Visible: true, Enabled: true} // toolkit defaults when traits are silent
	if v, ok := n.attr("id"); ok {
		e.ID = v
	}
	if v, ok := n.attr("test_id"); ok {
		e.TestID = v
	}
	if v, ok := n.attr("role"); ok {
		e.Role = v
	}
	// <app> nodes carry the app identity for everything beneath them.
	if n.XMLName.Local == "app" {
		id := e.ID
		if id == "" {
			id = e.TestID
		}
		appID = &id
	}
	if appID != nil {
		e.AppID = *appID
	}
	e.Text = strings.TrimSpace(n.Text)

	// Geometry: missing x/y/w/h is a parse warning, geometry stays zero;
	// malformed or overflowing values also warn (never silently corrupt).
	rectSet := false
	for _, g := range []struct {
		name string
		dst  *int32
	}{
		{"x", &e.Rect.X}, {"y", &e.Rect.Y}, {"width", &e.Rect.Width}, {"height", &e.Rect.Height},
	} {
		v, ok := n.attr(g.name)
		if !ok {
			continue
		}
		rectSet = true
		parsed, valid := atoi32(v)
		*g.dst = parsed
		if !valid {
			t.Warnings = append(t.Warnings, fmt.Sprintf("node %s/%s has malformed %s=%q", n.XMLName.Local, e.bestID(), g.name, v))
		}
	}
	if !rectSet && len(n.Children) == 0 && n.XMLName.Local != "root" {
		t.Warnings = append(t.Warnings, fmt.Sprintf("node %s/%s has no geometry", n.XMLName.Local, e.bestID()))
	}

	if v, ok := n.attr("focusable"); ok {
		e.Focusable = v == "true" || v == "1"
	}
	if v, ok := n.attr("focused"); ok {
		e.Focused = v == "true" || v == "1"
	}
	// Traits refine visibility/enablement (one entry per trait attribute).
	for _, tr := range n.Traits {
		for _, a := range tr.Attrs {
			switch a.Name.Local {
			case "visibility":
				e.Visible = a.Value == "visible"
			case "enableable":
				e.Enabled = a.Value == "enabled" || a.Value == "true"
			case "focusable":
				if a.Value == "setFocus" {
					e.Focusable = true
				}
			case "focused":
				if a.Value == "focused" || a.Value == "true" {
					e.Focused = true
				}
			}
		}
	}
	for i := range n.Children {
		child := n.Children[i].toElement(appID, t)
		// Zero-size children are preserved deliberately: virtualized lists
		// and offscreen content legitimately show zero geometry.
		e.Children = append(e.Children, child)
	}
	return e
}

func (e *Element) bestID() string {
	if e.TestID != "" {
		return e.TestID
	}
	if e.ID != "" {
		return e.ID
	}
	return e.Role
}

// atoi32 parses int32 geometry via the stdlib; the caller records a parse
// warning on failure (invariant 7: geometry never silently corrupts —
// malformed or overflowing values become zero WITH a warning).
func atoi32(s string) (int32, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(v), true
}

// Hash returns a stable digest of the tree's semantic content — element
// identity, geometry, flags (focused included), nesting. Identical trees
// hash identically; a focus flip changes the hash (invariant 11 input).
func (t *Tree) Hash() string {
	h := sha256.New()
	hashElement(h, &t.Root)
	return hex.EncodeToString(h.Sum(nil))
}

func hashElement(h io.Writer, e *Element) {
	fmt.Fprintf(h, "id=%s|tid=%s|role=%s|text=%s|app=%s|r=%d,%d,%dx%d|f=%t|F=%t|e=%t|v=%d;n=%d\n",
		e.ID, e.TestID, e.Role, e.Text, e.AppID,
		e.Rect.X, e.Rect.Y, e.Rect.Width, e.Rect.Height,
		e.Focusable, e.Focused, e.Enabled, b2i(e.Visible), len(e.Children))
	for i := range e.Children {
		hashElement(h, &e.Children[i])
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Filter returns the subtree of a single app (by app id) — multi-app page
// sources include the launcher; navigation usually wants one app.
func (t *Tree) Filter(appID string) *Tree {
	sub := &Tree{Warnings: t.Warnings}
	sub.Root = filterElement(&t.Root, appID)
	return sub
}

func filterElement(e *Element, appID string) Element {
	out := *e
	out.Children = nil
	if e.AppID == appID {
		// Keep this node and everything beneath it.
		for i := range e.Children {
			out.Children = append(out.Children, filterKeep(&e.Children[i]))
		}
		return out
	}
	// Not our app here — recurse looking for it.
	for i := range e.Children {
		c := filterElement(&e.Children[i], appID)
		if subtreeHasApp(&c, appID) {
			out.Children = append(out.Children, c.Children...)
		}
	}
	return out
}

func filterKeep(e *Element) Element {
	out := *e
	out.Children = nil
	for i := range e.Children {
		out.Children = append(out.Children, filterKeep(&e.Children[i]))
	}
	return out
}

func subtreeHasApp(e *Element, appID string) bool {
	if e.AppID == appID {
		return true
	}
	for i := range e.Children {
		if subtreeHasApp(&e.Children[i], appID) {
			return true
		}
	}
	return false
}

// Find returns the first element matching pred (depth-first, document
// order — stable for tests and navigation).
func (t *Tree) Find(pred func(*Element) bool) *Element {
	return findIn(&t.Root, pred)
}

func findIn(e *Element, pred func(*Element) bool) *Element {
	if pred(e) {
		return e
	}
	for i := range e.Children {
		if found := findIn(&e.Children[i], pred); found != nil {
			return found
		}
	}
	return nil
}

// Focused returns the focused element (nil if none).
func (t *Tree) Focused() *Element {
	return t.Find(func(e *Element) bool { return e.Focused })
}

// Focusables returns all focusable+visible elements (navigation candidates).
func (t *Tree) Focusables() []*Element {
	var out []*Element
	var walk func(*Element)
	walk = func(e *Element) {
		if e.Focusable && e.Visible {
			out = append(out, e)
		}
		for i := range e.Children {
			walk(&e.Children[i])
		}
	}
	walk(&t.Root)
	return out
}

// MarshalJSONString serializes the tree (golden round-trip form).
func (t *Tree) MarshalJSONString() (string, error) {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// FromJSON decodes a MarshalJSONString payload (round-trip law).
func FromJSON(s string) (*Tree, error) {
	var t Tree
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, fmt.Errorf("tree: malformed json tree: %w", err)
	}
	return &t, nil
}

// Summary is a bounded outline of the tree for CLI/MCP output (invariant
// 17): role, identity, focus marker, rect — capped at maxNodes with an
// explicit truncation marker.
func (t *Tree) Summary(maxNodes int) Summary {
	s := Summary{MaxNodes: maxNodes}
	var walk func(*Element, int)
	walk = func(e *Element, depth int) {
		if s.Truncated || len(s.Nodes) >= maxNodes {
			s.Truncated = true
			return
		}
		node := SummaryNode{
			Depth:     depth,
			Role:      e.Role,
			ID:        e.bestID(),
			Text:      e.Text,
			Rect:      e.Rect,
			Focused:   e.Focused,
			Focusable: e.Focusable,
		}
		s.Nodes = append(s.Nodes, node)
		for i := range e.Children {
			walk(&e.Children[i], depth+1)
		}
	}
	walk(&t.Root, 0)
	return s
}

// Summary is the bounded outline of a tree.
type Summary struct {
	MaxNodes  int           `json:"max_nodes"`
	Truncated bool          `json:"truncated,omitempty"`
	Nodes     []SummaryNode `json:"nodes"`
}

// SummaryNode is one outline entry.
type SummaryNode struct {
	Depth     int    `json:"d"`
	Role      string `json:"role,omitempty"`
	ID        string `json:"id,omitempty"`
	Text      string `json:"text,omitempty"`
	Rect      Rect   `json:"rect,omitempty"`
	Focused   bool   `json:"focused,omitempty"`
	Focusable bool   `json:"focusable,omitempty"`
}

// MarshalJSONString serializes the summary.
func (s Summary) MarshalJSONString() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// String renders the summary as an indented outline.
func (s Summary) String() string {
	var b strings.Builder
	for _, n := range s.Nodes {
		b.WriteString(strings.Repeat("  ", n.Depth))
		if n.Focused {
			b.WriteString("[*] ")
		} else if n.Focusable {
			b.WriteString("[ ] ")
		} else {
			b.WriteString(" ·  ")
		}
		if n.Role != "" {
			b.WriteString(n.Role)
		}
		if n.ID != "" {
			b.WriteString(" #" + n.ID)
		}
		if n.Text != "" {
			b.WriteString(" " + n.Text)
		}
		b.WriteString(fmt.Sprintf(" (%d,%d %dx%d)", n.Rect.X, n.Rect.Y, n.Rect.Width, n.Rect.Height))
		b.WriteString("\n")
	}
	if s.Truncated {
		fmt.Fprintf(&b, "… truncated at %d nodes\n", s.MaxNodes)
	}
	return b.String()
}

// SortWarnings deduplicates warnings deterministically.
func (t *Tree) SortWarnings() {
	sort.Strings(t.Warnings)
	t.Warnings = unique(t.Warnings)
}

func unique(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
