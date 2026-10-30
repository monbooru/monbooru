// Package search implements the Monbooru query language parser and SQL executor.
package search

import (
	"strings"

	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
)

type Expr interface {
	exprNode()
}

type AndExpr struct{ Left, Right Expr }

type OrExpr struct{ Left, Right Expr }

type NotExpr struct{ Expr Expr }

type TagExpr struct {
	Tag      string // normalized lowercase
	Wildcard string // "" | "prefix" | "suffix" | "substring"
}

type FilterExpr struct {
	Key string
	Val string
}

func (AndExpr) exprNode()    {}
func (OrExpr) exprNode()     {}
func (NotExpr) exprNode()    {}
func (TagExpr) exprNode()    {}
func (FilterExpr) exprNode() {}

func Parse(query string) Expr {
	tokens, _ := tokenize(query)
	p := &parser{tokens: tokens}
	exprs := p.parseAll()
	if len(exprs) == 0 {
		return nil
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = AndExpr{Left: result, Right: e}
	}
	return result
}

type tokenKind int

const (
	tokTag tokenKind = iota
	tokFilter
	tokNot
	tokOR
)

type token struct {
	kind tokenKind
	val  string
}

// open reports a quote the query never closes.
func tokenize(query string) (tokens []token, open bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false
	}

	i := 0
	for i < len(query) {
		if query[i] == ' ' || query[i] == '\t' {
			i++
			continue
		}

		if i+4 <= len(query) && strings.EqualFold(query[i:i+4], "not ") {
			tokens = append(tokens, token{kind: tokNot, val: "NOT"})
			i += 4
			continue
		}

		if query[i] == '-' && i+1 < len(query) && query[i+1] != ' ' {
			tokens = append(tokens, token{kind: tokNot, val: "-"})
			i++
			continue
		}

		j := i
		closed := true
		if query[j] == '"' {
			j, closed = skipQuoted(query, j)
		} else {
			for j < len(query) && query[j] != ' ' && query[j] != '\t' {
				opensQuote := query[j] == ':' || query[j] == '=' && strings.IndexByte(query[i:j], ':') > 0
				if opensQuote && j+1 < len(query) && query[j+1] == '"' {
					j, closed = skipQuoted(query, j+1)
					break
				}
				j++
			}
		}
		open = open || !closed
		term := query[i:j]
		i = j

		if strings.EqualFold(term, "or") {
			tokens = append(tokens, token{kind: tokOR, val: "OR"})
			continue
		}
		if strings.EqualFold(term, "and") {
			continue
		}

		if colonIdx := strings.IndexByte(term, ':'); colonIdx > 0 && term[0] != '"' {
			tokens = append(tokens, token{kind: tokFilter, val: term})
			continue
		}

		tokens = append(tokens, token{kind: tokTag, val: term})
	}
	return tokens, open
}

// j is the opening quote; the result is the index past the closing one.
func skipQuoted(query string, j int) (int, bool) {
	j++
	for j < len(query) && query[j] != '"' {
		if query[j] == '\\' && j+1 < len(query) {
			j += 2
			continue
		}
		j++
	}
	if j < len(query) {
		return j + 1, true
	}
	return j, false
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *parser) next() { p.pos++ }

func (p *parser) parseAll() []Expr {
	var exprs []Expr
	for {
		t := p.peek()
		if t == nil {
			break
		}

		// parseTerm returns nil at an OR, which would end the loop and
		// drop the rest of the query.
		if t.kind == tokOR {
			p.next()
			continue
		}

		expr := p.parseOperand()
		if expr == nil {
			continue
		}
		for {
			or := p.peek()
			if or == nil || or.kind != tokOR {
				break
			}
			p.next()
			right := p.parseOperand()
			if right == nil {
				break
			}
			expr = OrExpr{Left: expr, Right: right}
		}
		exprs = append(exprs, expr)
	}
	return exprs
}

// parseTerm stops at a NOT, so without this `a OR -b` would parse as `a -b`.
func (p *parser) parseOperand() Expr {
	if t := p.peek(); t != nil && t.kind == tokNot {
		p.next()
		inner := p.parseTerm()
		if inner == nil {
			return nil
		}
		return NotExpr{Expr: inner}
	}
	return p.parseTerm()
}

func (p *parser) parseTerm() Expr {
	t := p.peek()
	if t == nil {
		return nil
	}
	if t.kind == tokNot || t.kind == tokOR {
		return nil
	}
	p.next()

	switch t.kind {
	case tokFilter:
		colonIdx := strings.IndexByte(t.val, ':')
		key := strings.ToLower(t.val[:colonIdx])
		val := t.val[colonIdx+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = unescapeQuoted(val[1 : len(val)-1])
		} else if eq := strings.Index(val, `="`); eq > 0 && len(val) >= eq+3 && val[len(val)-1] == '"' {
			val = val[:eq+1] + unescapeQuoted(val[eq+2:len(val)-1])
		}
		return FilterExpr{Key: key, Val: val}

	case tokTag:
		tag := t.val
		if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
			tag = unescapeQuoted(tag[1 : len(tag)-1])
		}
		// Keeps the reserved `*` for the wildcard checks below.
		tag = tags.NormalizeTagName(tag)
		// Otherwise an all-* token becomes LIKE '%' and matches every tag.
		if strings.Trim(tag, "*") == "" {
			return TagExpr{Tag: "", Wildcard: ""}
		}
		if strings.HasPrefix(tag, "*") && strings.HasSuffix(tag, "*") && len(tag) > 2 {
			return TagExpr{Tag: tag[1 : len(tag)-1], Wildcard: "substring"}
		}
		if strings.HasSuffix(tag, "*") {
			return TagExpr{Tag: strings.TrimSuffix(tag, "*"), Wildcard: "prefix"}
		}
		if strings.HasPrefix(tag, "*") && len(tag) > 1 {
			return TagExpr{Tag: strings.TrimPrefix(tag, "*"), Wildcard: "suffix"}
		}
		return TagExpr{Tag: tag, Wildcard: ""}
	}
	return nil
}

func unescapeQuoted(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// QuoteValue escapes s so a key:"<s>" term parses back to s. Backslashes
// go first, or the escapes added for quotes would be escaped again.
func QuoteValue(s string) string {
	if !strings.ContainsAny(s, "\\\"") {
		return s
	}
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// Unreadable names what in query matches nothing as typed: a quote left
// open, and a comparison or date filter whose value does not parse.
func Unreadable(query string) []string {
	var out []string
	if _, open := tokenize(query); open {
		out = append(out, "unterminated quote")
	}
	WalkLeaves(Parse(query), func(e Expr) bool {
		if f, ok := e.(FilterExpr); ok && searchkw.IsOrdinal(f.Key) && filterBuilders[f.Key](&whereBuilder{}, f) == "1=0" {
			out = append(out, f.Key+":"+f.Val)
		}
		return true
	})
	return out
}
