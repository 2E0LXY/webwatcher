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
	Basis      string // BasisPerPerson, BasisTotal or BasisUnknown
	SoldOut    bool   // the page says the item is out of stock (JSON-LD availability)
}

// Price bases. A price per person and a price for the whole party must never
// be compared with each other.
const (
	BasisPerPerson = "per_person"
	BasisTotal     = "total"
	BasisUnknown   = "unknown"
)

var (
	basisPPRe    = regexp.MustCompile(`(?i)(\bper\s+(person|adult|traveller|traveler|passenger|guest|head)\b|\bpp\b|\bp\.p\.|\ba\s+head\b)`)
	basisTotalRe = regexp.MustCompile(`(?i)(\btotal\b|\bin\s+total\b|\bfor\s+(all\s+)?\d+\s+(adults?|people|persons|passengers|guests|travellers|travelers)\b|\bfor\s+the\s+(whole\s+)?(party|group)\b)`)
)

// basisFromText detects "per person" or "total" wording. The earliest
// indication wins; if neither appears the basis is unknown.
func basisFromText(s string) string {
	pp, tot := basisPPRe.FindStringIndex(s), basisTotalRe.FindStringIndex(s)
	switch {
	case pp != nil && tot != nil:
		if pp[0] < tot[0] {
			return BasisPerPerson
		}
		return BasisTotal
	case pp != nil:
		return BasisPerPerson
	case tot != nil:
		return BasisTotal
	}
	return BasisUnknown
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
		// "1.299" is one thousand two hundred and ninety-nine in European
		// notation; "0.999" and "1299.999" are decimals.
		if strings.Count(t, ".") == 1 && !(len(t)-dot-1 == 3 && dot >= 1 && dot <= 3 && t[:dot] != "0") {
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

const amountPat = `\d{1,3}(?:[.,]\d{3})+(?:[.,]\d{1,2})?|\d+(?:[.,]\d{1,2})?`

var (
	prefixMoneyRe = regexp.MustCompile(`([£€$])\s?(` + amountPat + `)`)
	suffixMoneyRe = regexp.MustCompile(`(` + amountPat + `)\s?(€|£|EUR|GBP|USD)`)
	bareMoneyRe   = regexp.MustCompile(amountPat)
)

type moneyMatch struct {
	start, end int
	pence      int64
	cur        string
}

// findMoneyAll returns every amount with a currency symbol or code in text,
// in document order. Prefix (£12.50, €1.299,50) and suffix (12,50 €) forms.
func findMoneyAll(text string) []moneyMatch {
	var out []moneyMatch
	for _, m := range prefixMoneyRe.FindAllStringSubmatchIndex(text, -1) {
		if p, ok := parseAmount(text[m[4]:m[5]]); ok {
			out = append(out, moneyMatch{m[0], m[1], p, currencyFromSymbol(text[m[2]:m[3]])})
		}
	}
	for _, m := range suffixMoneyRe.FindAllStringSubmatchIndex(text, -1) {
		if p, ok := parseAmount(text[m[2]:m[3]]); ok {
			out = append(out, moneyMatch{m[0], m[1], p, currencyFromSymbol(text[m[4]:m[5]])})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	// drop overlaps (a suffix match can swallow the tail of a prefix match)
	var clean []moneyMatch
	for _, m := range out {
		if len(clean) > 0 && m.start < clean[len(clean)-1].end {
			continue
		}
		clean = append(clean, m)
	}
	return clean
}

// findMoney returns the first amount in text, preferring ones with a currency symbol.
func findMoney(text string) (int64, string, bool) {
	if ms := findMoneyAll(text); len(ms) > 0 {
		return ms[0].pence, ms[0].cur, true
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
				return &Result{PricePence: p, Currency: cur, Method: "selector", Basis: basisFromText(nodeText(n))}, nil
			}
		}
	}
	if p, cur, ok := findMoney(nodeText(n)); ok {
		return &Result{PricePence: p, Currency: cur, Method: "selector", Basis: basisFromText(nodeText(n))}, nil
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

type ldOffer struct {
	pence   int64
	cur     string
	basis   string
	soldOut bool
}

var listPriceRe = regexp.MustCompile(`(?i)(listprice|strikethrough|srp|msrp|was)`)

// offerPrice reads the price of one Offer, preferring a sale price over a
// list ("was") price when price specifications are given.
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
	var specs []map[string]any
	switch ps := m["priceSpecification"].(type) {
	case map[string]any:
		specs = []map[string]any{ps}
	case []any:
		for _, x := range ps {
			if mm, ok := x.(map[string]any); ok {
				specs = append(specs, mm)
			}
		}
	}
	for pass := 0; pass < 2; pass++ {
		for _, sp := range specs {
			isList := listPriceRe.MatchString(anyToString(sp["priceType"]))
			if (pass == 0) == isList {
				continue // pass 0 takes sale prices, pass 1 falls back to list prices
			}
			if p, c, ok := try(sp); ok {
				return p, c, true
			}
		}
	}
	return 0, "", false
}

// offerText gathers the wording that may say what the price is per.
func offerText(m map[string]any) string {
	var parts []string
	var add func(mm map[string]any, depth int)
	add = func(mm map[string]any, depth int) {
		for _, k := range []string{"unitText", "name", "description", "priceType"} {
			if v := anyToString(mm[k]); v != "" {
				parts = append(parts, v)
			}
		}
		if depth > 0 {
			return
		}
		if rq, ok := mm["referenceQuantity"].(map[string]any); ok {
			add(rq, 1)
		}
		switch ps := mm["priceSpecification"].(type) {
		case map[string]any:
			add(ps, 1)
		case []any:
			for _, x := range ps {
				if sp, ok := x.(map[string]any); ok {
					add(sp, 1)
				}
			}
		}
	}
	add(m, 0)
	return strings.Join(parts, " ")
}

func soldOutText(v any) bool {
	l := strings.ToLower(anyToString(v))
	return strings.Contains(l, "outofstock") || strings.Contains(l, "soldout") || strings.Contains(l, "discontinued")
}

func offerFromMap(m map[string]any) (ldOffer, bool) {
	p, c, ok := offerPrice(m)
	if !ok {
		return ldOffer{}, false
	}
	return ldOffer{p, c, basisFromText(offerText(m)), soldOutText(m["availability"])}, true
}

// offerList collects every offer held in the value of an "offers" key.
func offerList(v any) []ldOffer {
	var out []ldOffer
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			out = append(out, offerList(e)...)
		}
	case map[string]any:
		if hasType(x, "AggregateOffer") {
			if sub := offerList(x["offers"]); len(sub) > 0 {
				return sub
			}
		}
		if o, ok := offerFromMap(x); ok {
			return []ldOffer{o}
		}
		if sub := offerList(x["offers"]); len(sub) > 0 {
			return sub
		}
	}
	return out
}

// ldOffers finds the offers of the first product-like node in the JSON-LD.
// Offers of related products further along are not mixed in.
func ldOffers(v any) []ldOffer {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if o := ldOffers(e); len(o) > 0 {
				return o
			}
		}
	case map[string]any:
		if hasType(x, "Offer", "AggregateOffer") {
			if o := offerList(x); len(o) > 0 {
				return o
			}
		}
		if o, ok := x["offers"]; ok {
			if l := offerList(o); len(l) > 0 {
				return l
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
				if l := ldOffers(x[k]); len(l) > 0 {
					return l
				}
			}
		}
	}
	return nil
}

// pickOffer chooses the lowest price among in-stock offers that share the
// first in-stock offer's currency. If every offer is sold out it reports that,
// with the first offer's price for information.
func pickOffer(offers []ldOffer) (ldOffer, bool) {
	var best *ldOffer
	for i := range offers {
		o := offers[i]
		if o.soldOut {
			continue
		}
		if best == nil {
			best = &o
			continue
		}
		if o.cur == best.cur && o.pence < best.pence {
			best = &o
		}
	}
	if best == nil {
		return offers[0], true
	}
	return *best, false
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
			// Malformed JSON-LD is skipped, not fatal: later blocks and the
			// other extraction methods still get their turn.
			if dec.Decode(&v) == nil {
				if offers := ldOffers(v); len(offers) > 0 {
					o, sold := pickOffer(offers)
					res = &Result{PricePence: o.pence, Currency: o.cur, Method: "jsonld", Basis: o.basis, SoldOut: sold}
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
			return &Result{PricePence: p, Currency: cur, Method: "meta", Basis: BasisUnknown}
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
	goodKwRe    = regexp.MustCompile(`(?i)\b(from|now|only|total|price|pp|per person|per adult|each)\b`)
	badKwRe     = regexp.MustCompile(`(?i)\b(was|save|saving|deposit|off|worth|rrp|fee|per month|a month|delivery|postage|shipping|reduced by|discount)\b|/mo\b`)
	badAnchorRe = regexp.MustCompile(`(?i)\b(was|old|strike|original|rrp|deposit|saving|save)\b`)
)

type candidate struct {
	pence int64
	cur   string
	basis string
	score int
}

// preScore looks at the words just before an amount. The nearest keyword
// decides, so "Was £899 Now £799" scores 899 down and 799 up.
func preScore(pre string) int {
	gi, bi := goodKwRe.FindAllStringIndex(pre, -1), badKwRe.FindAllStringIndex(pre, -1)
	lg, lb := -1, -1
	if len(gi) > 0 {
		lg = gi[len(gi)-1][0]
	}
	if len(bi) > 0 {
		lb = bi[len(bi)-1][0]
	}
	switch {
	case lg < 0 && lb < 0:
		return 0
	case lb > lg:
		return -4
	}
	return 3
}

// postScore looks at the words just after an amount. A "was" that introduces
// another amount ("Now £799 (was £899)") describes that amount, not this one.
func postScore(post string) int {
	score := 0
	if goodKwRe.MatchString(post) {
		score += 3
	}
	if loc := badKwRe.FindStringIndex(post); loc != nil && len(findMoneyAll(post[loc[1]:])) == 0 {
		score -= 4
	}
	return score
}

// extractRegex scans visible text for amounts with a currency symbol and
// scores them: "from/now/total" wording, price-like class names and headings
// score up; "was/save/deposit" wording and struck-through text score down.
// Highest score wins; ties go to the earliest in the document.
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
				for _, m := range findMoneyAll(text) {
					pre := tail + " " + text[:m.start]
					if len(pre) > 40 {
						pre = pre[len(pre)-40:]
					}
					post := text[m.end:]
					if len(post) > 25 {
						post = post[:25]
					}
					score := preScore(pre) + postScore(post)
					if priceCtx {
						score += 2
					}
					if headCtx {
						score++
					}
					if struckCtx {
						score -= 4
					}
					basis := basisFromText(post)
					if basis == BasisUnknown {
						basis = basisFromText(pre[max(0, len(pre)-25):])
					}
					cands = append(cands, candidate{m.pence, m.cur, basis, score})
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
	return &Result{PricePence: best.pence, Currency: best.cur, Method: "regex", Basis: best.basis}
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
