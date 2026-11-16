// Package search implements the Monbooru query language parser and SQL executor.
package search

import (
	"slices"
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

// Parse binds NOT tighter than AND, and AND tighter than OR. A query it
// can't fully read still parses as best it can; Unreadable says what it skipped.
func Parse(query string) Expr {
	expr, _ := parse(query)
	return expr
}

func parse(query string) (Expr, []string) {
	tokens, open := tokenize(query)
	p := &parser{tokens: tokens}
	if open {
		p.problem("unterminated quote")
	}
	var expr Expr
	for {
		expr = andOf(expr, p.parseOr())
		if p.peek() == nil {
			return expr, p.problems
		}
		// Only a ) stops parseOr at the top level.
		p.problem("unmatched )")
		p.next()
	}
}

type tokenKind int

const (
	tokTag tokenKind = iota
	tokFilter
	tokNot
	tokOR
	tokAND
	tokOpen
	tokClose
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

		// A paren is a group only standing alone, so saber_(fate) stays a tag.
		if (query[i] == '(' || query[i] == ')') && (i+1 == len(query) || query[i+1] == ' ' || query[i+1] == '\t') {
			if query[i] == ')' {
				tokens = append(tokens, token{kind: tokClose, val: ")"})
			} else {
				// `- (` negates the group like `-(` does.
				if n := len(tokens); n > 0 && tokens[n-1] == (token{kind: tokTag, val: "-"}) {
					tokens[n-1] = token{kind: tokNot, val: "-"}
				}
				tokens = append(tokens, token{kind: tokOpen, val: "("})
			}
			i++
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

		switch {
		case strings.EqualFold(term, "or"):
			tokens = append(tokens, token{kind: tokOR, val: "OR"})
			continue
		case strings.EqualFold(term, "and"):
			tokens = append(tokens, token{kind: tokAND, val: "AND"})
			continue
		case strings.EqualFold(term, "not"):
			tokens = append(tokens, token{kind: tokNot, val: "NOT"})
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
	tokens   []token
	pos      int
	problems []string
}

func (p *parser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *parser) next() { p.pos++ }

func (p *parser) problem(msg string) {
	if !slices.Contains(p.problems, msg) {
		p.problems = append(p.problems, msg)
	}
}

// startsOperand reports whether the next token can begin an operand.
func (p *parser) startsOperand() bool {
	t := p.peek()
	return t != nil && t.kind != tokOR && t.kind != tokAND && t.kind != tokClose
}

func andOf(left, right Expr) Expr {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	}
	return AndExpr{Left: left, Right: right}
}

func (p *parser) parseOr() Expr {
	expr := p.parseAnd()
	for t := p.peek(); t != nil && t.kind == tokOR; t = p.peek() {
		p.next()
		if expr == nil {
			p.problem("nothing before OR")
		}
		if !p.startsOperand() {
			p.problem("nothing after OR")
			continue
		}
		right := p.parseAnd()
		switch {
		case right == nil:
		case expr == nil:
			expr = right
		default:
			expr = OrExpr{Left: expr, Right: right}
		}
	}
	return expr
}

func (p *parser) parseAnd() Expr {
	var expr Expr
	for {
		t := p.peek()
		if t == nil || t.kind == tokOR || t.kind == tokClose {
			return expr
		}
		if t.kind == tokAND {
			p.next()
			if expr == nil {
				p.problem("nothing before AND")
			}
			if !p.startsOperand() {
				p.problem("nothing after AND")
			}
			continue
		}
		expr = andOf(expr, p.parseUnary())
	}
}

// parseUnary is only called where startsOperand holds, and always consumes.
func (p *parser) parseUnary() Expr {
	switch p.peek().kind {
	case tokNot:
		p.next()
		if !p.startsOperand() {
			p.problem("nothing after NOT")
			return nil
		}
		if inner := p.parseUnary(); inner != nil {
			return NotExpr{Expr: inner}
		}
		return nil
	case tokOpen:
		p.next()
		inner := p.parseOr()
		if t := p.peek(); t != nil && t.kind == tokClose {
			p.next()
		} else {
			p.problem("unclosed (")
		}
		if inner == nil {
			p.problem("empty ( )")
		}
		return inner
	}
	return p.parseTerm()
}

func (p *parser) parseTerm() Expr {
	t := p.peek()
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

// Unreadable names what in query is skipped or matches nothing as typed: a
// quote left open, a group or operator missing a side, and a comparison or
// date filter whose value does not parse.
func Unreadable(query string) []string {
	expr, out := parse(query)
	WalkLeaves(expr, func(e Expr) bool {
		if f, ok := e.(FilterExpr); ok && searchkw.IsOrdinal(f.Key) && filterBuilders[f.Key](&whereBuilder{}, f) == "1=0" {
			out = append(out, f.Key+":"+f.Val)
		}
		return true
	})
	return out
}
