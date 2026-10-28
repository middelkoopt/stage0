package gs316ep

import (
	"bytes"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// element is one HTML element in document order, with its attributes and the
// whitespace-collapsed text of everything inside it.
type element struct {
	tag   string
	attrs map[string]string
	text  string
}

func (e element) hasClass(name string) bool {
	return slices.Contains(strings.Fields(e.attrs["class"]), name)
}

func (e element) attr(name string) (string, bool) {
	v, ok := e.attrs[name]
	return v, ok
}

// scan parses a page and returns its elements in document order. The
// switch's HTML is loose (unclosed spans, odd comments); html.Parse repairs it
// the way a browser would.
func scan(page []byte) ([]element, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	var out []element
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		e := element{tag: n.Data, attrs: make(map[string]string, len(n.Attr)), text: text(n)}
		for _, a := range n.Attr {
			e.attrs[a.Key] = a.Val
		}
		out = append(out, e)
	}
	return out, nil
}

// text returns the collapsed text content of n (U+00A0 counts as space).
func text(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
