// Responsibilities: flatten explicitly permitted HTML formatting into CMS text.
// Dependencies: pinned x/net/html parser; callers: strict CMS payload admission.
// Invariants: no attributes survive and dangerous/unknown element subtrees vanish;
// renderer continues to escape the resulting text rather than interpreting HTML.
package service

import (
	"golang.org/x/net/html"
	"strings"
)

func cleanText(v string) string {
	// No HTML input avoids parser whitespace normalization of ordinary text.
	if !strings.Contains(v, "<") && !strings.Contains(v, "&") {
		return v
	}
	root, err := html.Parse(strings.NewReader(v))
	if err != nil {
		return ""
	}
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "html", "head", "body", "p", "div", "span", "strong", "em", "b", "i", "u", "s", "a", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "blockquote", "code", "pre":
			case "br":
				out.WriteByte('\n')
				return
			default:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "blockquote":
				out.WriteByte('\n')
			}
		}
	}
	walk(root)
	return strings.TrimSpace(out.String())
}
