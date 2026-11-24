package app

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Result is an extracted price.
type Result struct {
	PricePence int64
	Currency   string
	Method     string // selector, jsonld, meta, regex
}

// ExtractError carries a user-facing reason for failure.
type ExtractError struct {
	Reason string
	// BrowserNeeded is set when the page looks like an unrendered JS app shell.
	BrowserNeeded bool
}

func (e *ExtractError) Error() string { return e.Reason }

// parseAmount converts "1,299.50", "99", "1.299,50" or "12.5" to pence.
func parseAmount(s string) (int64, bool) {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == ',' {
			b.WriteRune(r)
		}
	}
	t := strings.Trim(b.String(), ".,")
	if t == "" {
		return 0, false
	}
	dot, comma := strings.LastIndex(t, "."), strings.LastIndex(t, ",")
	var intPart, frac string
	decAt := -1
	switch {
	case dot >= 0 && comma >= 0:
		decAt = max(dot, comma)
	case comma >= 0:
		if strings.Count(t, ",") == 1 && len(t)-comma-1 <= 2 {
			decAt = comma
		}
	case dot >= 0:
		if strings.Count(t, ".") == 1 {
			decAt = dot
		}
	}
	if decAt >= 0 {
		intPart, frac = t[:decAt], t[decAt+1:]
	} else {
		intPart = t
	}
	intPart = strings.NewReplacer(",", "", ".", "").Replace(intPart)
	frac = strings.NewReplacer(",", "", ".", "").Replace(frac)
	if intPart == "" {
		intPart = "0"
	}
	if len(intPart) > 10 {
		return 0, false
	}
	round := int64(0)
	if len(frac) > 2 {
		if frac[2] >= '5' {
			round = 1
		}
		frac = frac[:2]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var v int64
	for _, r := range intPart + frac {
		v = v*10 + int64(r-'0')
	}
	v += round
	if v <= 0 {
		return 0, false
	}
	return v, true
}

func currencyFromSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch {
	case s == "£" || s == "GBP":
		return "GBP"
	case s == "€" || s == "EUR":
		return "EUR"
	case s == "$" || s == "USD":
		return "USD"
	case len(s) == 3:
		return s
	}
	return ""
}

var (
	symMoneyRe  = regexp.MustCompile(`([£€$])\s?(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{1,2}))?`)
	bareMoneyRe = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d{1,2})?|\d+(?:\.\d{1,2})?`)
)

// findMoney returns the first amount in text, preferring ones with a currency symbol.
func findMoney(text string) (int64, string, bool) {
	if m := symMoneyRe.FindStringSubmatch(text); m != nil {
		if p, ok := parseAmount(m[2] + "." + m[3]); ok {
			return p, currencyFromSymbol(m[1]), true
		}
	}
	if m := bareMoneyRe.FindString(text); m != "" {
		if p, ok := parseAmount(m); ok {
			return p, "GBP", true
		}
	}
	return 0, "", false
}

// Extract finds a price in an HTML document. If selector is non-empty only
// the selector is used (no fallback), so a broken selector can never silently
// switch to a different number and raise a bogus alert.
func Extract(body []byte, selector string) (*Result, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, &ExtractError{Reason: "could not parse the page"}
	}
	if selector != "" {
		return extractSelector(doc, selector)
	}
	if r := extractJSONLD(doc); r != nil {
		return r, nil
	}
	if r := extractMeta(doc); r != nil {
		return r, nil
	}
	if r := extractRegex(doc); r != nil {
		return r, nil
	}
	if looksBrowserRendered(doc, body) {
		return nil, &ExtractError{Reason: "needs browser rendering (the page builds its prices with JavaScript)", BrowserNeeded: true}
	}
	return nil, &ExtractError{Reason: "no price found"}
}

func extractSelector(doc *html.Node, selector string) (*Result, error) {
	sel, err := ParseSelector(selector)
	if err != nil {
		return nil, &ExtractError{Reason: "invalid selector: " + err.Error()}
	}
	n := sel.Find(doc)
	if n == nil {
		if looksBrowserRendered(doc, nil) {
			return nil, &ExtractError{Reason: "selector matched nothing; the page may need browser rendering", BrowserNeeded: true}
		}
		return nil, &ExtractError{Reason: "selector matched nothing"}
	}
	for _, a := range []string{"content", "data-price", "data-price-amount", "data-amount"} {
		if v, ok := attrLookup(n, a); ok {
			if p, cur, ok := findMoney(v); ok {
				if c := attrVal(n, "data-currency"); c != "" {
					cur = currencyFromSymbol(c)
				}
				return &Result{p, cur, "selector"}, nil
			}
		}
	}
	if p, cur, ok := findMoney(nodeText(n)); ok {
		return &Result{p, cur, "selector"}, nil
	}
	return nil, &ExtractError{Reason: "selector matched but its text holds no price"}
}

func hasType(m map[string]any, want ...string) bool {
	var types []string
	switch t := m["@type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				types = append(types, s)
			}
		}
	}
	for _, t := range types {
		for _, w := range want {
			if strings.EqualFold(t, w) {
				return true
			}
		}
	}
	return false
}

func anyToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

func offerPrice(m map[string]any) (int64, string, bool) {
	keys := []string{"price", "lowPrice"}
	if hasType(m, "AggregateOffer") {
		keys = []string{"lowPrice", "price"}
	}
	cur := currencyFromSymbol(anyToString(m["priceCurrency"]))
	try := func(mm map[string]any) (int64, string, bool) {
		for _, k := range keys {
			if p, ok := parseAmount(anyToString(mm[k])); ok {
				c := currencyFromSymbol(anyToString(mm["priceCurrency"]))
				if c == "" {
					c = cur
				}
				if c == "" {
					c = "GBP"
				}
				return p, c, true
			}
		}
		return 0, "", false
	}
	if p, c, ok := try(m); ok {
		return p, c, true
	}
	switch ps := m["priceSpecification"].(type) {
	case map[string]any:
		return try(ps)
	case []any:
		for _, x := range ps {
			if mm, ok := x.(map[string]any); ok {
				if p, c, ok := try(mm); ok {
					return p, c, true
				}
			}
		}
	}
	return 0, "", false
}

func walkLD(v any) (int64, string, bool) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if p, c, ok := walkLD(e); ok {
				return p, c, true
			}
		}
	case map[string]any:
		if hasType(x, "Offer", "AggregateOffer") {
			if p, c, ok := offerPrice(x); ok {
				return p, c, true
			}
		}
		if o, ok := x["offers"]; ok {
			if p, c, ok := walkLD(o); ok {
				return p, c, true
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			if k != "offers" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch x[k].(type) {
			case map[string]any, []any:
				if p, c, ok := walkLD(x[k]); ok {
					return p, c, true
				}
			}
		}
	}
	return 0, "", false
}

func extractJSONLD(doc *html.Node) *Result {
	var res *Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if res != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "script" &&
			strings.Contains(strings.ToLower(attrVal(n, "type")), "ld+json") && n.FirstChild != nil {
			dec := json.NewDecoder(strings.NewReader(n.FirstChild.Data))
			dec.UseNumber()
			var v any
			if dec.Decode(&v) == nil {
				if p, c, ok := walkLD(v); ok {
					res = &Result{p, c, "jsonld"}
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return res
}

func extractMeta(doc *html.Node) *Result {
	var itemprop, og, prod, curItem, curOG, curProd string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			content := attrVal(n, "content")
			if ip := strings.ToLower(attrVal(n, "itemprop")); ip != "" {
				val := content
				if val == "" {
					val = strings.TrimSpace(nodeText(n))
				}
				switch ip {
				case "price":
					if itemprop == "" {
						itemprop = val
					}
				case "pricecurrency":
					if curItem == "" {
						curItem = val
					}
				}
			}
			if n.Data == "meta" {
				switch strings.ToLower(firstNonEmpty(attrVal(n, "property"), attrVal(n, "name"))) {
				case "og:price:amount":
					if og == "" {
						og = content
					}
				case "og:price:currency":
					curOG = content
				case "product:price:amount":
					if prod == "" {
						prod = content
					}
				case "product:price:currency":
					curProd = content
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for _, c := range []struct{ amt, cur string }{{itemprop, curItem}, {og, curOG}, {prod, curProd}} {
		if p, ok := parseAmount(c.amt); ok {
			cur := currencyFromSymbol(c.cur)
			if cur == "" {
				cur = "GBP"
			}
			return &Result{p, cur, "meta"}
		}
	}
	return nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

var (
	gbpRe       = regexp.MustCompile(`£\s?(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{2}))?`)
	goodPreRe   = regexp.MustCompile(`(?i)\b(from|now|only|total|price|pp|per person|per adult|each)\b`)
	badPreRe    = regexp.MustCompile(`(?i)\b(was|save|saving|deposit|off|worth|rrp|fee|per month|a month|delivery|postage|shipping|reduced by|discount)\b|/mo\b`)
	badAnchorRe = regexp.MustCompile(`(?i)\b(was|old|strike|original|rrp|deposit|saving|save)\b`)
)

type candidate struct {
	pence int64
	score int
}

// extractRegex scans visible text for £ amounts and scores them: "from/now/total"
// wording, price-like class names and headings score up; "was/save/deposit"
// wording and struck-through text score down. Highest score wins; ties go to
// the earliest in the document.
func extractRegex(doc *html.Node) *Result {
	var cands []candidate
	tail := "" // last characters of visible text before the current node
	var walk func(n *html.Node, priceCtx, headCtx, struckCtx bool)
	walk = func(n *html.Node, priceCtx, headCtx, struckCtx bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template", "head", "svg":
				return
			case "h1", "h2", "h3", "strong", "b":
				headCtx = true
			case "s", "del", "strike":
				struckCtx = true
			}
			ident := attrVal(n, "class") + " " + attrVal(n, "id")
			if strings.Contains(strings.ToLower(ident), "price") {
				priceCtx = true
			}
			if badAnchorRe.MatchString(strings.NewReplacer("-", " ", "_", " ").Replace(ident)) {
				struckCtx = true
			}
		}
		if n.Type == html.TextNode {
			text := strings.Join(strings.Fields(n.Data), " ")
			if text != "" {
				for _, m := range gbpRe.FindAllStringSubmatchIndex(text, -1) {
					p, ok := parseAmount(text[m[2]:m[3]] + "." + func() string {
						if m[4] >= 0 {
							return text[m[4]:m[5]]
						}
						return "00"
					}())
					if !ok {
						continue
					}
					pre := tail + " " + text[:m[0]]
					if len(pre) > 40 {
						pre = pre[len(pre)-40:]
					}
					post := text[m[1]:]
					if len(post) > 25 {
						post = post[:25]
					}
					score := 0
					if goodPreRe.MatchString(pre) || goodPreRe.MatchString(post) {
						score += 3
					}
					if priceCtx {
						score += 2
					}
					if headCtx {
						score++
					}
					if badPreRe.MatchString(pre) || badPreRe.MatchString(post) {
						score -= 4
					}
					if struckCtx {
						score -= 4
					}
					cands = append(cands, candidate{p, score})
				}
				tail += " " + text
				if len(tail) > 80 {
					tail = tail[len(tail)-80:]
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, priceCtx, headCtx, struckCtx)
		}
	}
	walk(doc, false, false, false)
	if len(cands) == 0 {
		return nil
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.score > best.score {
			best = c
		}
	}
	return &Result{best.pence, "GBP", "regex"}
}

// looksBrowserRendered guesses that a page is an empty JS application shell.
func looksBrowserRendered(doc *html.Node, body []byte) bool {
	text := nodeText(doc)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "enable javascript") || strings.Contains(lower, "javascript is required") ||
		strings.Contains(lower, "javascript is disabled") {
		return true
	}
	scripts, markers := 0, 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "script" {
				scripts++
			}
			switch attrVal(n, "id") {
			case "root", "app", "__next", "__nuxt", "react-root", "ng-app":
				markers++
			}
			if _, ok := attrLookup(n, "ng-version"); ok {
				markers++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return len(text) < 2500 && (markers > 0 || scripts >= 5)
}
