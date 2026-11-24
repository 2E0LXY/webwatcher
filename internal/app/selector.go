package app

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// A deliberately small CSS selector subset:
//   tag, *, .class, #id, [attr], [attr=v], [attr^=v], [attr$=v], [attr*=v], [attr~=v],
//   compounds of those (div.price#main[data-x]), descendant (space) and child (>) combinators.

type attrSel struct{ name, op, val string }

type compound struct {
	tag     string
	id      string
	classes []string
	attrs   []attrSel
	child   bool // combinator to the previous compound is '>'
}

type Selector []compound

func isIdent(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ParseSelector parses the supported subset.
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 {
		return nil, errors.New("selector must be 1 to 200 characters")
	}
	var sel Selector
	i := 0
	child := false
	ident := func() string {
		st := i
		for i < len(s) && isIdent(s[i]) {
			i++
		}
		return s[st:i]
	}
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i < len(s) && s[i] == '>' {
			if len(sel) == 0 || child {
				return nil, errors.New("unexpected '>' in selector")
			}
			child = true
			i++
			continue
		}
		if i >= len(s) {
			break
		}
		var c compound
		c.child = child
		child = false
		start := i
		if s[i] == '*' {
			i++
		} else if isIdent(s[i]) {
			c.tag = strings.ToLower(ident())
		}
	loop:
		for i < len(s) {
			switch s[i] {
			case '#':
				i++
				if c.id = ident(); c.id == "" {
					return nil, errors.New("empty #id in selector")
				}
			case '.':
				i++
				cl := ident()
				if cl == "" {
					return nil, errors.New("empty .class in selector")
				}
				c.classes = append(c.classes, cl)
			case '[':
				i++
				name := ident()
				if name == "" {
					return nil, errors.New("bad attribute selector")
				}
				a := attrSel{name: strings.ToLower(name)}
				if i < len(s) && s[i] != ']' {
					for _, op := range []string{"^=", "$=", "*=", "~=", "="} {
						if strings.HasPrefix(s[i:], op) {
							a.op = op
							i += len(op)
							break
						}
					}
					if a.op == "" {
						return nil, errors.New("unsupported attribute operator")
					}
					if i < len(s) && (s[i] == '"' || s[i] == '\'') {
						q := s[i]
						i++
						st := i
						for i < len(s) && s[i] != q {
							i++
						}
						if i >= len(s) {
							return nil, errors.New("unterminated quote in selector")
						}
						a.val = s[st:i]
						i++
					} else {
						a.val = ident()
					}
				}
				if i >= len(s) || s[i] != ']' {
					return nil, errors.New("missing ] in selector")
				}
				i++
				c.attrs = append(c.attrs, a)
			default:
				break loop
			}
		}
		if i == start {
			return nil, fmt.Errorf("unsupported selector syntax near %q", s[i:min(len(s), i+10)])
		}
		if i < len(s) && s[i] != ' ' && s[i] != '>' {
			return nil, fmt.Errorf("unsupported selector syntax near %q", s[i:min(len(s), i+10)])
		}
		sel = append(sel, c)
	}
	if len(sel) == 0 || child {
		return nil, errors.New("incomplete selector")
	}
	return sel, nil
}

func (c compound) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if c.tag != "" && n.Data != c.tag {
		return false
	}
	if c.id != "" && attrVal(n, "id") != c.id {
		return false
	}
	if len(c.classes) > 0 {
		have := strings.Fields(attrVal(n, "class"))
		for _, want := range c.classes {
			found := false
			for _, h := range have {
				if h == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	for _, a := range c.attrs {
		v, ok := attrLookup(n, a.name)
		if !ok {
			return false
		}
		switch a.op {
		case "":
		case "=":
			ok = v == a.val
		case "^=":
			ok = a.val != "" && strings.HasPrefix(v, a.val)
		case "$=":
			ok = a.val != "" && strings.HasSuffix(v, a.val)
		case "*=":
			ok = a.val != "" && strings.Contains(v, a.val)
		case "~=":
			ok = false
			for _, f := range strings.Fields(v) {
				if f == a.val {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func attrLookup(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func attrVal(n *html.Node, name string) string { v, _ := attrLookup(n, name); return v }

func (s Selector) matchAt(n *html.Node, idx int) bool {
	if !s[idx].matches(n) {
		return false
	}
	if idx == 0 {
		return true
	}
	if s[idx].child {
		return n.Parent != nil && s.matchAt(n.Parent, idx-1)
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if s.matchAt(p, idx-1) {
			return true
		}
	}
	return false
}

// Find returns the first element in document order matching the selector.
func (s Selector) Find(root *html.Node) *html.Node {
	var found *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && s.matchAt(n, len(s)-1) {
			found = n
			return
		}
		for c := n.FirstChild; c != nil && found == nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

// nodeText returns whitespace-collapsed visible text of n.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template":
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
