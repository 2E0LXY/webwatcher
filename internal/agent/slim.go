package agent

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// SlimHTML shrinks an oversized page by dropping what the server's price
// extraction never reads: style, svg, noscript, comments, and scripts other than
// JSON-LD. It is used only when the page is over the size cap.
func SlimHTML(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return src
	}
	var prune func(n *html.Node)
	prune = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			drop := false
			switch c.Type {
			case html.CommentNode:
				drop = true
			case html.ElementNode:
				switch c.Data {
				case "style", "svg", "noscript", "link", "iframe":
					drop = true
				case "script":
					drop = !isJSONLD(c)
				}
			}
			if drop {
				n.RemoveChild(c)
			} else {
				prune(c)
			}
			c = next
		}
	}
	prune(doc)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return src
	}
	return buf.String()
}

func isJSONLD(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "type" && strings.Contains(strings.ToLower(a.Val), "ld+json") {
			return true
		}
	}
	return false
}
