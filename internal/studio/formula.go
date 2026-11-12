package studio

// Formulas: a small spreadsheet-style language for calculated fields and root filters.
// Formulas compile to T-SQL. Only the functions listed here exist, so a formula cannot
// run arbitrary SQL. The compiler also avoids common runtime failures: division by
// zero gives an empty value, mismatched types are refused, and time-dependent
// functions are rejected because documents only rebuild when their data changes.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
)

// FormulaError is a problem in a formula, with the byte offset it was found at.
type FormulaError struct {
	Pos int
	Msg string
}

func (e *FormulaError) Error() string { return e.Msg }

func errAt(src string, pos int, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if pos >= len(src) {
		return &FormulaError{Pos: pos, Msg: msg + " (at the end)"}
	}
	end := pos + 16
	if end > len(src) {
		end = len(src)
	}
	return &FormulaError{Pos: pos, Msg: fmt.Sprintf("%s (near %q)", msg, src[pos:end])}
}

// ---- lexer ---------------------------------------------------------------------------

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tOp
)

type token struct {
	kind   tokKind
	text   string // number text, string value, identifier or operator
	pos    int
	quoted bool // identifier written in [brackets]
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }

func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isDigit(c):
			j := i
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			if j+1 < len(src) && src[j] == '.' && isDigit(src[j+1]) {
				j++
				for j < len(src) && isDigit(src[j]) {
					j++
				}
			}
			if j < len(src) && isIdentStart(src[j]) {
				return nil, errAt(src, j, "unexpected %q after a number", src[j])
			}
			out = append(out, token{kind: tNum, text: src[i:j], pos: i})
			i = j
		case c == '\'' || c == '"':
			var b strings.Builder
			j := i + 1
			for {
				if j >= len(src) {
					return nil, errAt(src, i, "this text has no closing %c", c)
				}
				if src[j] == c {
					if j+1 < len(src) && src[j+1] == c { // doubled quote: a literal quote
						b.WriteByte(c)
						j += 2
						continue
					}
					j++
					break
				}
				b.WriteByte(src[j])
				j++
			}
			out = append(out, token{kind: tStr, text: b.String(), pos: i})
			i = j
		case c == '[':
			var b strings.Builder
			j := i + 1
			for {
				if j >= len(src) {
					return nil, errAt(src, i, "this name has no closing ]")
				}
				if src[j] == ']' {
					if j+1 < len(src) && src[j+1] == ']' {
						b.WriteByte(']')
						j += 2
						continue
					}
					j++
					break
				}
				b.WriteByte(src[j])
				j++
			}
			if b.Len() == 0 {
				return nil, errAt(src, i, "empty name []")
			}
			out = append(out, token{kind: tIdent, text: b.String(), pos: i, quoted: true})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{kind: tIdent, text: src[i:j], pos: i})
			i = j
		default:
			if i+1 < len(src) {
				switch two := src[i : i+2]; two {
				case "<=", ">=", "<>", "!=":
					out = append(out, token{kind: tOp, text: two, pos: i})
					i += 2
					continue
				}
			}
			if strings.IndexByte("+-*/%&=<>(),.", c) >= 0 {
				out = append(out, token{kind: tOp, text: string(c), pos: i})
				i++
				continue
			}
			return nil, errAt(src, i, "unexpected character %q", c)
		}
	}
	return append(out, token{kind: tEOF, pos: len(src)}), nil
}

// ---- parser --------------------------------------------------------------------------

type node interface{ at() int }

type (
	numLit struct {
		p    int
		text string
	}
	strLit struct {
		p   int
		val string
	}
	boolLit struct {
		p   int
		val bool
	}
	nullLit struct{ p int }
	refNode struct {
		p     int
		parts []string // name, or qualifier and name
	}
	unaryNode struct {
		p  int
		op string
		x  node
	} // "-" or "NOT"
	binaryNode struct {
		p    int
		op   string // + - * / % & = <> < <= > >= AND OR
		l, r node
	}
	inNode struct {
		p    int
		x    node
		list []node
		not  bool
	}
	isNullNode struct {
		p   int
		x   node
		not bool
	}
	callNode struct {
		p    int
		name string // upper case
		args []node
	}
)

func (n *numLit) at() int     { return n.p }
func (n *strLit) at() int     { return n.p }
func (n *boolLit) at() int    { return n.p }
func (n *nullLit) at() int    { return n.p }
func (n *refNode) at() int    { return n.p }
func (n *unaryNode) at() int  { return n.p }
func (n *binaryNode) at() int { return n.p }
func (n *inNode) at() int     { return n.p }
func (n *isNullNode) at() int { return n.p }
func (n *callNode) at() int   { return n.p }

type parser struct {
	src  string
	toks []token
	i    int
}

func parseFormula(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	if p.peek().kind == tEOF {
		return nil, &FormulaError{Msg: "the formula is empty"}
	}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, errAt(src, t.pos, "unexpected %s", describeTok(t))
	}
	return n, nil
}

func describeTok(t token) string {
	switch t.kind {
	case tNum:
		return "number " + t.text
	case tStr:
		return "text"
	case tIdent:
		return "name " + t.text
	case tEOF:
		return "end"
	}
	return "'" + t.text + "'"
}

func (p *parser) peek() token         { return p.toks[p.i] }
func (p *parser) peekAt(k int) token  { return p.toks[min(p.i+k, len(p.toks)-1)] }
func (p *parser) isOp(op string) bool { t := p.peek(); return t.kind == tOp && t.text == op }
func (p *parser) isKw(kw string) bool { return isKw(p.peek(), kw) }
func isKw(t token, kw string) bool {
	return t.kind == tIdent && !t.quoted && strings.EqualFold(t.text, kw)
}
func (p *parser) fail(f string, a ...any) error {
	return errAt(p.src, p.peek().pos, f, a...)
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) or() (node, error) {
	l, err := p.and()
	for err == nil && p.isKw("OR") {
		t := p.next()
		var r node
		if r, err = p.and(); err == nil {
			l = &binaryNode{p: t.pos, op: "OR", l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) and() (node, error) {
	l, err := p.not()
	for err == nil && p.isKw("AND") {
		t := p.next()
		var r node
		if r, err = p.not(); err == nil {
			l = &binaryNode{p: t.pos, op: "AND", l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) not() (node, error) {
	if p.isKw("NOT") {
		t := p.next()
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &unaryNode{p: t.pos, op: "NOT", x: x}, nil
	}
	return p.cmp()
}

func (p *parser) cmp() (node, error) {
	l, err := p.concat()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	switch {
	case t.kind == tOp && strings.Contains("|=|<>|!=|<|<=|>|>=|", "|"+t.text+"|"):
		p.next()
		r, err := p.concat()
		if err != nil {
			return nil, err
		}
		op := t.text
		if op == "!=" {
			op = "<>"
		}
		return &binaryNode{p: t.pos, op: op, l: l, r: r}, nil
	case isKw(t, "IS"):
		p.next()
		not := false
		if p.isKw("NOT") {
			p.next()
			not = true
		}
		if !p.isKw("NULL") {
			return nil, p.fail("expected NULL after IS")
		}
		p.next()
		return &isNullNode{p: t.pos, x: l, not: not}, nil
	case isKw(t, "IN") || (isKw(t, "NOT") && isKw(p.peekAt(1), "IN")):
		not := false
		if p.isKw("NOT") {
			p.next()
			not = true
		}
		p.next() // IN
		if !p.isOp("(") {
			return nil, p.fail("expected ( after IN")
		}
		p.next()
		var list []node
		for {
			item, err := p.concat()
			if err != nil {
				return nil, err
			}
			list = append(list, item)
			if p.isOp(",") {
				p.next()
				continue
			}
			if !p.isOp(")") {
				return nil, p.fail("expected , or ) in the IN list")
			}
			p.next()
			break
		}
		return &inNode{p: t.pos, x: l, list: list, not: not}, nil
	}
	return l, nil
}

func (p *parser) concat() (node, error) {
	l, err := p.add()
	for err == nil && p.isOp("&") {
		t := p.next()
		var r node
		if r, err = p.add(); err == nil {
			l = &binaryNode{p: t.pos, op: "&", l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) add() (node, error) {
	l, err := p.mul()
	for err == nil && (p.isOp("+") || p.isOp("-")) {
		t := p.next()
		var r node
		if r, err = p.mul(); err == nil {
			l = &binaryNode{p: t.pos, op: t.text, l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) mul() (node, error) {
	l, err := p.unary()
	for err == nil && (p.isOp("*") || p.isOp("/") || p.isOp("%")) {
		t := p.next()
		var r node
		if r, err = p.unary(); err == nil {
			l = &binaryNode{p: t.pos, op: t.text, l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) unary() (node, error) {
	if p.isOp("-") {
		t := p.next()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{p: t.pos, op: "-", x: x}, nil
	}
	if p.isOp("+") {
		p.next()
		return p.unary()
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return &numLit{p: t.pos, text: t.text}, nil
	case tStr:
		return &strLit{p: t.pos, val: t.text}, nil
	case tEOF:
		return nil, errAt(p.src, t.pos, "the formula ends too early")
	case tOp:
		if t.text == "(" {
			n, err := p.or()
			if err != nil {
				return nil, err
			}
			if !p.isOp(")") {
				return nil, p.fail("expected )")
			}
			p.next()
			return n, nil
		}
		return nil, errAt(p.src, t.pos, "unexpected '%s'", t.text)
	}
	// identifier: keyword, function call or field reference
	if !t.quoted {
		switch strings.ToUpper(t.text) {
		case "TRUE":
			return &boolLit{p: t.pos, val: true}, nil
		case "FALSE":
			return &boolLit{p: t.pos, val: false}, nil
		case "NULL":
			return &nullLit{p: t.pos}, nil
		case "AND", "OR", "NOT", "IN", "IS":
			return nil, errAt(p.src, t.pos, "unexpected %s", strings.ToUpper(t.text))
		}
		if p.isOp("(") {
			p.next()
			var args []node
			if !p.isOp(")") {
				for {
					a, err := p.or()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if !p.isOp(",") {
						break
					}
					p.next()
				}
			}
			if !p.isOp(")") {
				return nil, p.fail("expected ) to close %s(", strings.ToUpper(t.text))
			}
			p.next()
			return &callNode{p: t.pos, name: strings.ToUpper(t.text), args: args}, nil
		}
	}
	parts := []string{t.text}
	if p.isOp(".") {
		p.next()
		n := p.next()
		if n.kind != tIdent {
			return nil, errAt(p.src, n.pos, "expected a column name after %s.", t.text)
		}
		parts = append(parts, n.text)
	}
	return &refNode{p: t.pos, parts: parts}, nil
}

// ---- types ---------------------------------------------------------------------------

type tyClass string

const (
	tyAny   tyClass = ""
	tyInt   tyClass = "int"
	tyDec   tyClass = "decimal"
	tyFloat tyClass = "float"
	tyText  tyClass = "text"
	tyDate  tyClass = "date"
	tyTime  tyClass = "datetime"
	tyBool  tyClass = "bool"
)

func classOf(sqlType string) tyClass {
	t := strings.ToLower(sqlType)
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = t[:i]
	}
	switch t {
	case "tinyint", "smallint", "int", "bigint":
		return tyInt
	case "decimal", "numeric", "money", "smallmoney":
		return tyDec
	case "float", "real":
		return tyFloat
	case "char", "varchar", "nchar", "nvarchar", "text", "ntext", "sysname":
		return tyText
	case "date":
		return tyDate
	case "datetime", "datetime2", "smalldatetime", "datetimeoffset":
		return tyTime
	case "bit":
		return tyBool
	}
	return tyAny
}

func isNum(t tyClass) bool  { return t == tyInt || t == tyDec || t == tyFloat }
func isDate(t tyClass) bool { return t == tyDate || t == tyTime }

func describeTy(t tyClass) string {
	switch t {
	case tyInt, tyDec, tyFloat:
		return "a number"
	case tyText:
		return "text"
	case tyDate, tyTime:
		return "a date"
	case tyBool:
		return "true/false"
	}
	return "a value"
}

func compatible(a, b tyClass) bool {
	switch {
	case a == tyAny || b == tyAny || a == b:
		return true
	case isNum(a) && isNum(b), isDate(a) && isDate(b):
		return true
	case (a == tyBool && b == tyInt) || (a == tyInt && b == tyBool):
		return true // flag = 1
	}
	return false
}

// ---- compiler ------------------------------------------------------------------------

type vkind int

const (
	kValue vkind = iota
	kPred        // a SQL condition, not a value
)

type cexpr struct {
	sql  string
	kind vkind
	ty   tyClass
	sub  bool // contains a subquery (a summary)
	lit  *strLit
}

type relScope struct {
	r     Relation
	t     *Table
	alias string
	list  bool // many rows per document: only usable inside summaries
}

type fieldSource struct {
	rel *relScope // nil: the root table
	col Column
}

type aggCtx struct {
	rel   *relScope
	alias string
}

// formulaScope resolves names for one design and collects what the compiled SQL needs.
type formulaScope struct {
	d         *Design
	s         *Snapshot
	root      *Table
	rootKey   string
	rels      map[string]*relScope
	relOrder  []string
	calcs     map[string]*Calc
	fields    map[string]fieldSource
	filter    bool
	joins     map[string]bool
	nsub      int
	expanding []string
	cache     map[string]cexpr
	src       string // formula being compiled (for error positions)
}

const calculatedFragment = "calculated"

func newFormulaScope(d *Design, s *Snapshot) (*formulaScope, error) {
	root, ok := s.Table(d.Root.Table)
	if !ok || len(root.PK) != 1 {
		return nil, fmt.Errorf("choose a root table with a single-column primary key first")
	}
	sc := &formulaScope{d: d, s: s, root: root, rootKey: root.PK[0], rels: map[string]*relScope{}, calcs: map[string]*Calc{},
		fields: map[string]fieldSource{}, joins: map[string]bool{}, cache: map[string]cexpr{}}
	for _, f := range d.Root.Fields {
		if c, ok := root.Column(f.Column); ok {
			sc.fields[strings.ToLower(f.Output())] = fieldSource{col: c}
		}
	}
	for _, r := range d.Related {
		t, ok := s.Table(r.Table)
		if !ok {
			continue
		}
		rs := &relScope{r: r, t: t, alias: "f_" + r.ID, list: r.Shape == ShapeList || r.Via != nil}
		sc.rels[strings.ToLower(r.ID)] = rs
		sc.relOrder = append(sc.relOrder, strings.ToLower(r.ID))
		if !rs.list && !r.Hidden && r.Path == "" {
			for _, f := range r.Fields {
				if c, ok := t.Column(f.Column); ok {
					sc.fields[strings.ToLower(f.Output())] = fieldSource{rel: rs, col: c}
				}
			}
		}
	}
	for i := range d.Calculated {
		sc.calcs[strings.ToLower(d.Calculated[i].Name)] = &d.Calculated[i]
	}
	return sc, nil
}

// compileCalc compiles a calculated field to a SQL value.
func (sc *formulaScope) compileCalc(c Calc) (cexpr, error) {
	k := strings.ToLower(c.Name)
	if ce, ok := sc.cache[k]; ok {
		return ce, nil
	}
	sc.expanding = append(sc.expanding, k)
	defer func() { sc.expanding = sc.expanding[:len(sc.expanding)-1] }()
	n, err := parseFormula(c.Expr)
	if err != nil {
		return cexpr{}, err
	}
	saved := sc.src
	sc.src = c.Expr
	defer func() { sc.src = saved }()
	ce, err := sc.value(n, nil)
	if err != nil {
		return cexpr{}, err
	}
	sc.cache[k] = ce
	return ce, nil
}

// compileFilter compiles the root filter to a SQL condition over the root table (alias t).
func (sc *formulaScope) compileFilter(src string) (string, error) {
	n, err := parseFormula(src)
	if err != nil {
		return "", err
	}
	sc.filter, sc.src = true, src
	defer func() { sc.filter = false }()
	p, err := sc.pred(n, nil)
	if err != nil {
		return "", err
	}
	return p.sql, nil
}

// fragmentSQL is the calculated fields' fragment: one row per document.
func (sc *formulaScope) fragmentSQL(cols []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT r.id AS _root,\n       %s\nFROM {{roots}} r\nJOIN %s t ON t.%s = r.id", strings.Join(cols, ",\n       "),
		config.MustParseTableName(sc.root.Name).Quoted(), q(sc.rootKey))
	for _, id := range sc.relOrder {
		if !sc.joins[sc.rels[id].r.ID] {
			continue
		}
		rs := sc.rels[id]
		fmt.Fprintf(&b, "\nLEFT JOIN %s %s ON %s", config.MustParseTableName(rs.t.Name).Quoted(), rs.alias, sc.parentCond(rs.alias, rs.r.Join))
	}
	return b.String()
}

func (sc *formulaScope) err(n node, f string, a ...any) error { return errAt(sc.src, n.at(), f, a...) }

func (sc *formulaScope) value(n node, agg *aggCtx) (cexpr, error) {
	e, err := sc.expr(n, agg)
	if err != nil || e.kind == kValue {
		return e, err
	}
	return cexpr{sql: "(CASE WHEN " + e.sql + " THEN CAST(1 AS bit) WHEN NOT " + e.sql + " THEN CAST(0 AS bit) END)", ty: tyBool, sub: e.sub}, nil
}

func (sc *formulaScope) pred(n node, agg *aggCtx) (cexpr, error) {
	e, err := sc.expr(n, agg)
	if err != nil || e.kind == kPred {
		return e, err
	}
	if e.ty == tyBool {
		return cexpr{sql: "(" + e.sql + " = 1)", kind: kPred, sub: e.sub}, nil
	}
	return cexpr{}, sc.err(n, "expected a condition (something true or false), but this is %s", describeTy(e.ty))
}

func (sc *formulaScope) expr(n node, agg *aggCtx) (cexpr, error) {
	switch n := n.(type) {
	case *numLit:
		ty := tyInt
		if strings.Contains(n.text, ".") {
			ty = tyDec
		} else if _, err := strconv.ParseInt(n.text, 10, 64); err != nil {
			return cexpr{}, sc.err(n, "the number %s is too large", n.text)
		}
		return cexpr{sql: n.text, ty: ty}, nil
	case *strLit:
		return cexpr{sql: "N'" + strings.ReplaceAll(n.val, "'", "''") + "'", ty: tyText, lit: n}, nil
	case *boolLit:
		if n.val {
			return cexpr{sql: "CAST(1 AS bit)", ty: tyBool}, nil
		}
		return cexpr{sql: "CAST(0 AS bit)", ty: tyBool}, nil
	case *nullLit:
		return cexpr{sql: "NULL"}, nil
	case *refNode:
		return sc.ref(n, agg)
	case *callNode:
		return sc.call(n, agg)
	case *isNullNode:
		v, err := sc.value(n.x, agg)
		if err != nil {
			return v, err
		}
		return cexpr{sql: "(" + v.sql + " IS " + map[bool]string{true: "NOT ", false: ""}[n.not] + "NULL)", kind: kPred, sub: v.sub}, nil
	case *unaryNode:
		if n.op == "NOT" {
			p, err := sc.pred(n.x, agg)
			if err != nil {
				return p, err
			}
			return cexpr{sql: "(NOT " + p.sql + ")", kind: kPred, sub: p.sub}, nil
		}
		v, err := sc.value(n.x, agg)
		if err != nil {
			return v, err
		}
		if v.ty != tyAny && !isNum(v.ty) {
			return cexpr{}, sc.err(n, "minus needs a number, but this is %s", describeTy(v.ty))
		}
		return cexpr{sql: "(-" + v.sql + ")", ty: v.ty, sub: v.sub}, nil
	case *inNode:
		x, err := sc.value(n.x, agg)
		if err != nil {
			return x, err
		}
		items := make([]string, len(n.list))
		sub := x.sub
		for i, it := range n.list {
			v, err := sc.value(it, agg)
			if err != nil {
				return v, err
			}
			if !compatibleCmp(x, v) {
				return cexpr{}, sc.err(it, "compares %s with %s", describeTy(x.ty), describeTy(v.ty))
			}
			items[i], sub = v.sql, sub || v.sub
		}
		not := ""
		if n.not {
			not = "NOT "
		}
		return cexpr{sql: "(" + x.sql + " " + not + "IN (" + strings.Join(items, ", ") + "))", kind: kPred, sub: sub}, nil
	case *binaryNode:
		return sc.binary(n, agg)
	}
	return cexpr{}, fmt.Errorf("unsupported formula node %T", n)
}

// compatibleCmp also accepts a date compared with text that is a valid date literal.
func compatibleCmp(a, b cexpr) bool {
	if compatible(a.ty, b.ty) {
		return true
	}
	if isDate(a.ty) && b.lit != nil {
		return validDate(b.lit.val)
	}
	if isDate(b.ty) && a.lit != nil {
		return validDate(a.lit.val)
	}
	return false
}

func validDate(s string) bool {
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999"} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

func (sc *formulaScope) binary(n *binaryNode, agg *aggCtx) (cexpr, error) {
	switch n.op {
	case "AND", "OR":
		l, err := sc.pred(n.l, agg)
		if err != nil {
			return l, err
		}
		r, err := sc.pred(n.r, agg)
		if err != nil {
			return r, err
		}
		return cexpr{sql: "(" + l.sql + " " + n.op + " " + r.sql + ")", kind: kPred, sub: l.sub || r.sub}, nil
	case "=", "<>", "<", "<=", ">", ">=":
		_, lNull := n.l.(*nullLit)
		_, rNull := n.r.(*nullLit)
		if lNull || rNull { // x = NULL is never true in SQL; mean IS NULL
			if n.op != "=" && n.op != "<>" {
				return cexpr{}, sc.err(n, "cannot compare with NULL using %s", n.op)
			}
			other := n.l
			if lNull {
				other = n.r
			}
			return sc.expr(&isNullNode{p: n.p, x: other, not: n.op == "<>"}, agg)
		}
		l, err := sc.value(n.l, agg)
		if err != nil {
			return l, err
		}
		r, err := sc.value(n.r, agg)
		if err != nil {
			return r, err
		}
		if !compatibleCmp(l, r) {
			hint := ""
			if (isDate(l.ty) && r.lit != nil) || (isDate(r.ty) && l.lit != nil) {
				hint = "; write dates as \"2024-12-31\""
			}
			return cexpr{}, sc.err(n, "compares %s with %s%s", describeTy(l.ty), describeTy(r.ty), hint)
		}
		return cexpr{sql: "(" + l.sql + " " + n.op + " " + r.sql + ")", kind: kPred, sub: l.sub || r.sub}, nil
	case "&":
		l, err := sc.value(n.l, agg)
		if err != nil {
			return l, err
		}
		r, err := sc.value(n.r, agg)
		if err != nil {
			return r, err
		}
		return cexpr{sql: "CONCAT(" + l.sql + ", " + r.sql + ")", ty: tyText, sub: l.sub || r.sub}, nil
	}
	l, err := sc.value(n.l, agg)
	if err != nil {
		return l, err
	}
	r, err := sc.value(n.r, agg)
	if err != nil {
		return r, err
	}
	for _, side := range []struct {
		e cexpr
		n node
	}{{l, n.l}, {r, n.r}} {
		switch {
		case side.e.ty == tyText && n.op == "+":
			return cexpr{}, sc.err(side.n, "use & to join text (+ adds numbers)")
		case isDate(side.e.ty):
			return cexpr{}, sc.err(side.n, "arithmetic on dates is not supported; use DATEDIFF or DATEADD")
		case side.e.ty != tyAny && !isNum(side.e.ty):
			return cexpr{}, sc.err(side.n, "%s needs numbers, but this is %s", n.op, describeTy(side.e.ty))
		}
	}
	sub := l.sub || r.sub
	switch n.op {
	case "/": // real division; dividing by zero gives an empty value instead of an error
		return cexpr{sql: "(CAST(" + l.sql + " AS float) / NULLIF(CAST(" + r.sql + " AS float), 0))", ty: tyFloat, sub: sub}, nil
	case "%":
		if (l.ty != tyAny && l.ty != tyInt) || (r.ty != tyAny && r.ty != tyInt) {
			return cexpr{}, sc.err(n, "%% needs whole numbers")
		}
		return cexpr{sql: "(" + l.sql + " % NULLIF(" + r.sql + ", 0))", ty: tyInt, sub: sub}, nil
	}
	ty := tyAny
	switch {
	case l.ty == tyFloat || r.ty == tyFloat:
		ty = tyFloat
	case l.ty == tyDec || r.ty == tyDec:
		ty = tyDec
	case l.ty == tyInt && r.ty == tyInt:
		ty = tyInt
	}
	return cexpr{sql: "(" + l.sql + " " + n.op + " " + r.sql + ")", ty: ty, sub: sub}, nil
}

func (sc *formulaScope) ref(n *refNode, agg *aggCtx) (cexpr, error) {
	if len(n.parts) == 1 {
		name := n.parts[0]
		k := strings.ToLower(name)
		self := len(sc.expanding) > 0 && sc.expanding[len(sc.expanding)-1] == k
		if c, ok := sc.calcs[k]; ok && !self && !sc.filter {
			for _, e := range sc.expanding {
				if e == k {
					return cexpr{}, sc.err(n, "calculated fields use each other in a loop (%s)", strings.Join(append(append([]string(nil), sc.expanding...), k), " → "))
				}
			}
			saved := sc.src
			ce, err := sc.compileCalc(*c)
			sc.src = saved
			if err != nil {
				return cexpr{}, sc.err(n, "uses %s, which has an error: %v", c.Name, err)
			}
			if ce.sub && agg != nil {
				return cexpr{}, sc.err(n, "%s contains a summary, and a summary cannot contain another summary", c.Name)
			}
			return ce, nil
		}
		if f, ok := sc.fields[k]; ok && (f.rel == nil || !sc.filter) {
			return sc.column(f.rel, f.col), nil
		}
		if col, ok := sc.root.Column(name); ok {
			return sc.column(nil, col), nil
		}
		if rs, ok := sc.rels[k]; ok {
			if rs.list {
				return cexpr{}, sc.err(n, "%s is a list; summarise it, for example COUNT(%s)", rs.r.ID, rs.r.ID)
			}
			return cexpr{}, sc.err(n, "%s is a table; pick one of its columns, for example %s.%s", rs.r.ID, rs.r.ID, firstColumn(rs.t))
		}
		if hint := suggest(name, sc.names()); hint != "" || sc.filter {
			return cexpr{}, sc.err(n, "unknown field %s%s", name, hint)
		}
		for _, id := range sc.relOrder { // a related table's column written without its table
			if rs := sc.rels[id]; func() bool { _, ok := rs.t.Column(name); return ok }() {
				return cexpr{}, sc.err(n, "unknown field %s; for the column of %s, write %s.%s", name, rs.r.ID, rs.r.ID, name)
			}
		}
		return cexpr{}, sc.err(n, "unknown field %s", name)
	}
	qual, colName := n.parts[0], n.parts[1]
	if strings.EqualFold(qual, short(sc.root.Name)) {
		col, ok := sc.root.Column(colName)
		if !ok {
			return cexpr{}, sc.err(n, "%s has no column %s%s", short(sc.root.Name), colName, suggest(colName, columnNames(sc.root)))
		}
		return sc.column(nil, col), nil
	}
	rs, ok := sc.rels[strings.ToLower(qual)]
	if !ok {
		cands := []string{short(sc.root.Name)}
		for _, id := range sc.relOrder {
			cands = append(cands, sc.rels[id].r.ID)
		}
		return cexpr{}, sc.err(n, "unknown table %s; use the name of a related table in this design%s", qual, suggest(qual, cands))
	}
	if sc.filter {
		return cexpr{}, sc.err(n, "a filter can use only columns of %s, because changes to other tables do not re-check it", short(sc.root.Name))
	}
	col, ok := rs.t.Column(colName)
	if !ok {
		return cexpr{}, sc.err(n, "%s has no column %s%s", rs.r.ID, colName, suggest(colName, columnNames(rs.t)))
	}
	if rs.list {
		if agg == nil {
			return cexpr{}, sc.err(n, "%s is a list (many rows per document); summarise it, for example MAX(%s.%s) or COUNT(%s)", rs.r.ID, rs.r.ID, col.Name, rs.r.ID)
		}
		if agg.rel != rs {
			return cexpr{}, sc.err(n, "a summary can use one list at a time; this one already uses %s", agg.rel.r.ID)
		}
		return cexpr{sql: agg.alias + "." + q(col.Name), ty: classOf(col.Type)}, nil
	}
	return sc.column(rs, col), nil
}

func (sc *formulaScope) column(rs *relScope, col Column) cexpr {
	if rs == nil {
		return cexpr{sql: "t." + q(col.Name), ty: classOf(col.Type)}
	}
	sc.joins[rs.r.ID] = true
	return cexpr{sql: rs.alias + "." + q(col.Name), ty: classOf(col.Type)}
}

// parentCond joins alias's columns (To) to the root's columns (From).
func (sc *formulaScope) parentCond(alias string, join []JoinPair) string {
	parts := make([]string, len(join))
	for i, j := range join {
		parent := "t." + q(j.From)
		if len(join) == 1 && strings.EqualFold(j.From, sc.rootKey) {
			parent = "r.id"
		}
		parts[i] = fmt.Sprintf("%s.%s = %s", alias, q(j.To), parent)
	}
	return strings.Join(parts, " AND ")
}

// listSource is the FROM and WHERE of a summary over one list relation.
func (sc *formulaScope) listSource(rs *relScope, alias string) (from, where string) {
	rel := config.MustParseTableName(rs.t.Name).Quoted()
	if rs.r.Via == nil {
		return rel + " " + alias, sc.parentCond(alias, rs.r.Join)
	}
	bt, _ := sc.s.Table(rs.r.Via.Table)
	b := alias + "b"
	on := make([]string, len(rs.r.Join))
	for i, j := range rs.r.Join {
		on[i] = fmt.Sprintf("%s.%s = %s.%s", alias, q(j.To), b, q(j.From))
	}
	return fmt.Sprintf("%s %s JOIN %s %s ON %s", config.MustParseTableName(bt.Name).Quoted(), b, rel, alias, strings.Join(on, " AND ")),
		sc.parentCond(b, rs.r.Via.Join)
}

// listsIn returns the list relations a summary's argument refers to.
func (sc *formulaScope) listsIn(n node) []*relScope {
	var out []*relScope
	seen := map[*relScope]bool{}
	var walk func(node)
	walk = func(n node) {
		switch n := n.(type) {
		case *refNode:
			if len(n.parts) == 2 {
				if rs, ok := sc.rels[strings.ToLower(n.parts[0])]; ok && rs.list && !seen[rs] {
					seen[rs] = true
					out = append(out, rs)
				}
			}
		case *unaryNode:
			walk(n.x)
		case *binaryNode:
			walk(n.l)
			walk(n.r)
		case *inNode:
			walk(n.x)
			for _, it := range n.list {
				walk(it)
			}
		case *isNullNode:
			walk(n.x)
		case *callNode:
			for _, a := range n.args {
				walk(a)
			}
		}
	}
	walk(n)
	return out
}

// bareListColumn finds a name in n that is not a field but is a column of a list, such
// as amount written for rates.amount.
func (sc *formulaScope) bareListColumn(n node) (col, rel string) {
	var walk func(node) bool
	walk = func(n node) bool {
		switch n := n.(type) {
		case *refNode:
			if len(n.parts) != 1 {
				return false
			}
			k := strings.ToLower(n.parts[0])
			if _, ok := sc.calcs[k]; ok {
				return false
			}
			if _, ok := sc.fields[k]; ok {
				return false
			}
			if _, ok := sc.root.Column(n.parts[0]); ok {
				return false
			}
			for _, id := range sc.relOrder {
				if rs := sc.rels[id]; rs.list {
					if c, ok := rs.t.Column(n.parts[0]); ok {
						col, rel = c.Name, rs.r.ID
						return true
					}
				}
			}
		case *unaryNode:
			return walk(n.x)
		case *binaryNode:
			return walk(n.l) || walk(n.r)
		case *inNode:
			if walk(n.x) {
				return true
			}
			for _, it := range n.list {
				if walk(it) {
					return true
				}
			}
		case *isNullNode:
			return walk(n.x)
		case *callNode:
			for _, a := range n.args {
				if walk(a) {
					return true
				}
			}
		}
		return false
	}
	walk(n)
	return col, rel
}

var aggregates = map[string]bool{"COUNT": true, "COUNTIF": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}

var timeFuncs = map[string]bool{"NOW": true, "TODAY": true, "GETDATE": true, "GETUTCDATE": true, "SYSDATETIME": true,
	"SYSUTCDATETIME": true, "CURRENT_TIMESTAMP": true, "RAND": true, "NEWID": true}

func (sc *formulaScope) aggregate(n *callNode, outer *aggCtx) (cexpr, error) {
	if sc.filter {
		return cexpr{}, sc.err(n, "a filter can use only columns of %s", short(sc.root.Name))
	}
	if outer != nil {
		return cexpr{}, sc.err(n, "a summary cannot contain another summary")
	}
	if len(n.args) != 1 {
		if (n.name == "MIN" || n.name == "MAX") && len(n.args) == 2 {
			return cexpr{}, sc.err(n, "%s summarises a list, for example %s(inclusions.severity); to pick between two values use IF(a > b, a, b)", n.name, n.name)
		}
		return cexpr{}, sc.err(n, "%s takes one argument, for example %s(%s)", n.name, n.name, sc.exampleList(n.name))
	}
	arg := n.args[0]
	if r, ok := arg.(*refNode); ok && len(r.parts) == 1 && n.name == "COUNT" {
		if rs, ok := sc.rels[strings.ToLower(r.parts[0])]; ok {
			if !rs.list {
				return cexpr{}, sc.err(n, "%s is not a list; COUNT counts the rows of a list", rs.r.ID)
			}
			sc.nsub++
			from, where := sc.listSource(rs, fmt.Sprintf("s%d", sc.nsub))
			return cexpr{sql: "(SELECT COUNT_BIG(*) FROM " + from + " WHERE " + where + ")", ty: tyInt, sub: true}, nil
		}
	}
	lists := sc.listsIn(arg)
	switch len(lists) {
	case 0:
		if col, rel := sc.bareListColumn(arg); col != "" {
			return cexpr{}, sc.err(n, "%s is a column of %s; write %s.%s", col, rel, rel, col)
		}
		return cexpr{}, sc.err(n, "%s needs a list to summarise, for example %s(%s)", n.name, n.name, sc.exampleList(n.name))
	case 1:
	default:
		return cexpr{}, sc.err(n, "a summary can use one list at a time; this one uses %s and %s", lists[0].r.ID, lists[1].r.ID)
	}
	rs := lists[0]
	sc.nsub++
	ctx := &aggCtx{rel: rs, alias: fmt.Sprintf("s%d", sc.nsub)}
	from, where := sc.listSource(rs, ctx.alias)
	if n.name == "COUNTIF" {
		cond, err := sc.pred(arg, ctx)
		if err != nil {
			return cond, err
		}
		if cond.sub {
			return cexpr{}, sc.err(n, "a summary cannot contain another summary")
		}
		return cexpr{sql: "(SELECT COUNT_BIG(*) FROM " + from + " WHERE " + where + " AND " + cond.sql + ")", ty: tyInt, sub: true}, nil
	}
	v, err := sc.value(arg, ctx)
	if err != nil {
		return v, err
	}
	if v.sub {
		return cexpr{}, sc.err(n, "a summary cannot contain another summary")
	}
	inner, ty := "", v.ty
	switch n.name {
	case "COUNT":
		inner, ty = "COUNT_BIG("+v.sql+")", tyInt
	case "SUM", "AVG":
		if v.ty != tyAny && !isNum(v.ty) {
			return cexpr{}, sc.err(n, "%s needs numbers, but this is %s%s", n.name, describeTy(v.ty), map[bool]string{true: "; to count true values use COUNTIF", false: ""}[v.ty == tyBool])
		}
		// Exact arithmetic keeps documents identical however rows are ordered.
		switch {
		case n.name == "SUM" && v.ty == tyInt:
			inner = "SUM(CAST(" + v.sql + " AS bigint))"
		case v.ty == tyDec:
			inner = n.name + "(" + v.sql + ")"
		default:
			inner, ty = n.name+"(CAST("+v.sql+" AS decimal(38, 6)))", tyDec
		}
	case "MIN", "MAX":
		if v.ty == tyBool {
			return cexpr{}, sc.err(n, "%s of true/false values is not supported; use COUNTIF", n.name)
		}
		inner = n.name + "(" + v.sql + ")"
	}
	return cexpr{sql: "(SELECT " + inner + " FROM " + from + " WHERE " + where + ")", ty: ty, sub: true}, nil
}

func (sc *formulaScope) exampleList(fn string) string {
	for _, id := range sc.relOrder {
		rs := sc.rels[id]
		if !rs.list {
			continue
		}
		col := valueColumn(rs)
		if fn == "COUNTIF" {
			return rs.r.ID + "." + col + " > 0"
		}
		return rs.r.ID + "." + col
	}
	return "list.column"
}

func intArg(sc *formulaScope, n node, what string, minVal int64) (int64, error) {
	neg := false
	if u, ok := n.(*unaryNode); ok && u.op == "-" {
		neg, n = true, u.x
	}
	lit, ok := n.(*numLit)
	if !ok || strings.Contains(lit.text, ".") {
		return 0, sc.err(n, "%s must be a whole number written in the formula", what)
	}
	v, _ := strconv.ParseInt(lit.text, 10, 64)
	if neg {
		v = -v
	}
	if v < minVal {
		return 0, sc.err(n, "%s must be at least %d", what, minVal)
	}
	return v, nil
}

var dateParts = map[string]bool{"year": true, "quarter": true, "month": true, "week": true, "day": true, "hour": true, "minute": true, "second": true}

func datePart(sc *formulaScope, n node) (string, error) {
	lit, ok := n.(*strLit)
	if !ok || !dateParts[strings.ToLower(lit.val)] {
		return "", sc.err(n, "the unit must be one of \"year\", \"quarter\", \"month\", \"week\", \"day\", \"hour\", \"minute\" or \"second\"")
	}
	return strings.ToLower(lit.val), nil
}

type funcSpec struct {
	min, max int // argument counts; max -1 = any
	help     string
}

// scalarFuncs lists every non-summary function (summaries are in aggregates).
var scalarFuncs = map[string]funcSpec{
	"IF": {2, 3, "IF(condition, value_if_true, value_if_false)"}, "COALESCE": {2, -1, "COALESCE(value, fallback, ...)"},
	"NULLIF": {2, 2, "NULLIF(value, empty_when_equal_to)"}, "ISBLANK": {1, 1, "ISBLANK(value)"},
	"ROUND": {1, 2, "ROUND(number, digits)"}, "FLOOR": {1, 1, "FLOOR(number)"}, "CEILING": {1, 1, "CEILING(number)"},
	"ABS": {1, 1, "ABS(number)"}, "SQRT": {1, 1, "SQRT(number)"},
	"UPPER": {1, 1, "UPPER(text)"}, "LOWER": {1, 1, "LOWER(text)"}, "TRIM": {1, 1, "TRIM(text)"}, "LEN": {1, 1, "LEN(text)"},
	"LEFT": {2, 2, "LEFT(text, count)"}, "RIGHT": {2, 2, "RIGHT(text, count)"}, "MID": {3, 3, "MID(text, start, count)"},
	"SUBSTRING": {3, 3, "SUBSTRING(text, start, count)"}, "REPLACE": {3, 3, "REPLACE(text, find, replace_with)"},
	"CONCAT": {1, -1, "CONCAT(value, ...)"}, "CONTAINS": {2, 2, "CONTAINS(text, part)"},
	"STARTSWITH": {2, 2, "STARTSWITH(text, prefix)"}, "ENDSWITH": {2, 2, "ENDSWITH(text, suffix)"},
	"YEAR": {1, 1, "YEAR(date)"}, "MONTH": {1, 1, "MONTH(date)"}, "DAY": {1, 1, "DAY(date)"},
	"DATEDIFF": {3, 3, "DATEDIFF(\"day\", start, end)"}, "DATEADD": {3, 3, "DATEADD(\"day\", count, date)"},
	"NUMBER": {1, 1, "NUMBER(text)"}, "INTEGER": {1, 1, "INTEGER(value)"}, "TEXT": {1, 1, "TEXT(value)"},
}

func (sc *formulaScope) call(n *callNode, agg *aggCtx) (cexpr, error) {
	if aggregates[n.name] {
		return sc.aggregate(n, agg)
	}
	if timeFuncs[n.name] {
		return cexpr{}, sc.err(n, "%s() is not allowed: documents rebuild only when their data changes, so its value would go stale", n.name)
	}
	spec, ok := scalarFuncs[n.name]
	if !ok {
		names := make([]string, 0, len(scalarFuncs)+len(aggregates))
		for k := range scalarFuncs {
			names = append(names, k)
		}
		for k := range aggregates {
			names = append(names, k)
		}
		return cexpr{}, sc.err(n, "unknown function %s%s", n.name, suggest(n.name, names))
	}
	if len(n.args) < spec.min || (spec.max >= 0 && len(n.args) > spec.max) {
		return cexpr{}, sc.err(n, "wrong number of arguments; use %s", spec.help)
	}
	vals := func(from int) ([]cexpr, bool, error) {
		var out []cexpr
		sub := false
		for _, a := range n.args[from:] {
			v, err := sc.value(a, agg)
			if err != nil {
				return nil, false, err
			}
			out, sub = append(out, v), sub || v.sub
		}
		return out, sub, nil
	}
	switch n.name {
	case "IF":
		cond, err := sc.pred(n.args[0], agg)
		if err != nil {
			return cond, err
		}
		vs, sub, err := vals(1)
		if err != nil {
			return cexpr{}, err
		}
		els := "NULL"
		if len(vs) == 2 {
			if !compatible(vs[0].ty, vs[1].ty) {
				return cexpr{}, sc.err(n, "IF gives %s in one case and %s in the other", describeTy(vs[0].ty), describeTy(vs[1].ty))
			}
			els = vs[1].sql
		}
		ty := vs[0].ty
		if ty == tyAny && len(vs) == 2 {
			ty = vs[1].ty
		}
		return cexpr{sql: "(CASE WHEN " + cond.sql + " THEN " + vs[0].sql + " ELSE " + els + " END)", ty: ty, sub: sub || cond.sub}, nil
	case "ISBLANK":
		vs, sub, err := vals(0)
		if err != nil {
			return cexpr{}, err
		}
		if vs[0].ty == tyText { // empty or blank text counts as blank too
			return cexpr{sql: "(NULLIF(LTRIM(RTRIM(" + vs[0].sql + ")), N'') IS NULL)", kind: kPred, sub: sub}, nil
		}
		return cexpr{sql: "(" + vs[0].sql + " IS NULL)", kind: kPred, sub: sub}, nil
	case "CONTAINS", "STARTSWITH", "ENDSWITH":
		vs, sub, err := vals(0)
		if err != nil {
			return cexpr{}, err
		}
		s, part := vs[0].sql, vs[1].sql
		sqls := map[string]string{
			"CONTAINS":   "(CHARINDEX(" + part + ", " + s + ") > 0)",
			"STARTSWITH": "(CHARINDEX(" + part + ", " + s + ") = 1)",
			"ENDSWITH":   "(RIGHT(" + s + ", LEN(" + part + ")) = " + part + ")",
		}
		return cexpr{sql: sqls[n.name], kind: kPred, sub: sub}, nil
	case "LEFT", "RIGHT":
		cnt, err := intArg(sc, n.args[1], "the count", 0)
		if err != nil {
			return cexpr{}, err
		}
		v, err := sc.value(n.args[0], agg)
		if err != nil {
			return v, err
		}
		return cexpr{sql: fmt.Sprintf("%s(%s, %d)", n.name, v.sql, cnt), ty: tyText, sub: v.sub}, nil
	case "MID", "SUBSTRING":
		start, err := intArg(sc, n.args[1], "the start", 1)
		if err != nil {
			return cexpr{}, err
		}
		cnt, err := intArg(sc, n.args[2], "the count", 0)
		if err != nil {
			return cexpr{}, err
		}
		v, err := sc.value(n.args[0], agg)
		if err != nil {
			return v, err
		}
		return cexpr{sql: fmt.Sprintf("SUBSTRING(%s, %d, %d)", v.sql, start, cnt), ty: tyText, sub: v.sub}, nil
	case "ROUND":
		digits := int64(0)
		if len(n.args) == 2 {
			d, err := intArg(sc, n.args[1], "the number of digits", -10)
			if err != nil {
				return cexpr{}, err
			}
			digits = d
		}
		v, err := sc.value(n.args[0], agg)
		if err != nil {
			return v, err
		}
		if v.ty != tyAny && !isNum(v.ty) {
			return cexpr{}, sc.err(n, "ROUND needs a number, but this is %s", describeTy(v.ty))
		}
		return cexpr{sql: fmt.Sprintf("ROUND(%s, %d)", v.sql, digits), ty: v.ty, sub: v.sub}, nil
	case "DATEDIFF", "DATEADD":
		unit, err := datePart(sc, n.args[0])
		if err != nil {
			return cexpr{}, err
		}
		if n.name == "DATEADD" {
			cnt, err := intArg(sc, n.args[1], "the count", -1_000_000)
			if err != nil {
				return cexpr{}, err
			}
			d, err := sc.value(n.args[2], agg)
			if err != nil {
				return d, err
			}
			if d.ty != tyAny && !isDate(d.ty) {
				return cexpr{}, sc.err(n.args[2], "DATEADD needs a date, but this is %s", describeTy(d.ty))
			}
			return cexpr{sql: fmt.Sprintf("DATEADD(%s, %d, %s)", unit, cnt, d.sql), ty: d.ty, sub: d.sub}, nil
		}
		vs, sub, err := vals(1)
		if err != nil {
			return cexpr{}, err
		}
		for i, v := range vs {
			if v.ty != tyAny && !isDate(v.ty) && v.lit == nil {
				return cexpr{}, sc.err(n.args[i+1], "DATEDIFF needs dates, but this is %s", describeTy(v.ty))
			}
		}
		return cexpr{sql: fmt.Sprintf("DATEDIFF_BIG(%s, %s, %s)", unit, vs[0].sql, vs[1].sql), ty: tyInt, sub: sub}, nil
	}

	vs, sub, err := vals(0)
	if err != nil {
		return cexpr{}, err
	}
	args := make([]string, len(vs))
	for i, v := range vs {
		args[i] = v.sql
	}
	num := func() error {
		for i, v := range vs {
			if v.ty != tyAny && !isNum(v.ty) {
				return sc.err(n.args[i], "%s needs a number, but this is %s", n.name, describeTy(v.ty))
			}
		}
		return nil
	}
	date := func() error {
		if v := vs[0]; v.ty != tyAny && !isDate(v.ty) {
			return sc.err(n.args[0], "%s needs a date, but this is %s", n.name, describeTy(v.ty))
		}
		return nil
	}
	out := cexpr{sub: sub}
	switch n.name {
	case "COALESCE", "NULLIF":
		for _, v := range vs[1:] {
			if !compatible(vs[0].ty, v.ty) {
				return cexpr{}, sc.err(n, "%s mixes %s and %s", n.name, describeTy(vs[0].ty), describeTy(v.ty))
			}
		}
		out.sql, out.ty = n.name+"("+strings.Join(args, ", ")+")", vs[0].ty
	case "FLOOR", "CEILING", "ABS":
		if err := num(); err != nil {
			return cexpr{}, err
		}
		out.sql, out.ty = n.name+"("+args[0]+")", vs[0].ty
	case "SQRT":
		if err := num(); err != nil {
			return cexpr{}, err
		}
		out.sql, out.ty = "(CASE WHEN "+args[0]+" >= 0 THEN SQRT(CAST("+args[0]+" AS float)) END)", tyFloat
	case "UPPER", "LOWER":
		out.sql, out.ty = n.name+"("+args[0]+")", tyText
	case "TRIM":
		out.sql, out.ty = "LTRIM(RTRIM("+args[0]+"))", tyText
	case "LEN":
		out.sql, out.ty = "LEN("+args[0]+")", tyInt
	case "REPLACE":
		out.sql, out.ty = "REPLACE("+strings.Join(args, ", ")+")", tyText
	case "CONCAT":
		if len(args) == 1 {
			args = append(args, "N''")
		}
		out.sql, out.ty = "CONCAT("+strings.Join(args, ", ")+")", tyText
	case "YEAR", "MONTH", "DAY":
		if err := date(); err != nil {
			return cexpr{}, err
		}
		out.sql, out.ty = n.name+"("+args[0]+")", tyInt
	case "NUMBER":
		out.sql, out.ty = "TRY_CAST("+args[0]+" AS float)", tyFloat
	case "INTEGER":
		out.sql, out.ty = "TRY_CAST("+args[0]+" AS bigint)", tyInt
	case "TEXT":
		if isDate(vs[0].ty) {
			out.sql = "CONVERT(nvarchar(33), " + args[0] + ", 126)"
		} else {
			out.sql = "CAST(" + args[0] + " AS nvarchar(4000))"
		}
		out.ty = tyText
	default:
		return cexpr{}, sc.err(n, "unknown function %s", n.name)
	}
	return out, nil
}

// ---- helpers -------------------------------------------------------------------------

func (sc *formulaScope) names() []string {
	var out []string
	for k := range sc.fields {
		out = append(out, k)
	}
	for _, c := range sc.root.Columns {
		out = append(out, c.Name)
	}
	if !sc.filter {
		for k := range sc.calcs {
			out = append(out, k)
		}
	}
	return out
}

func columnNames(t *Table) []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

func firstColumn(t *Table) string {
	for _, c := range t.Columns {
		if !containsAll(t.PK, []string{c.Name}) {
			return c.Name
		}
	}
	if len(t.Columns) > 0 {
		return t.Columns[0].Name
	}
	return "column"
}

// valueColumn picks a column worth summarising: numeric if possible, and neither a
// key nor a column the relation joins on.
func valueColumn(rs *relScope) string {
	skip := append([]string(nil), rs.t.PK...)
	for _, j := range rs.r.Join {
		skip = append(skip, j.From, j.To)
	}
	var first string
	for _, c := range rs.t.Columns {
		if containsAll(skip, []string{c.Name}) {
			continue
		}
		if isNum(classOf(c.Type)) {
			return c.Name
		}
		if first == "" {
			first = c.Name
		}
	}
	if first != "" {
		return first
	}
	return firstColumn(rs.t)
}

// suggest returns " (did you mean x?)" for the closest candidate, if any is close.
func suggest(name string, candidates []string) string {
	best, bestD := "", 3
	sort.Strings(candidates)
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(name), strings.ToLower(c)); d < bestD {
			best, bestD = c, d
		}
	}
	if best == "" {
		return ""
	}
	return " (did you mean " + best + "?)"
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
