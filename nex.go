// Substantial copy-and-paste from src/pkg/regexp.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
)
import (
	"go/parser"
	"go/printer"
	"go/token"
)

type rule struct {
	regex             []rune
	code              string
	index int
}
type Error string

func (e Error) String() string { return string(e) }

var (
	ErrInternal            = Error("internal error")
	ErrUnmatchedLpar       = Error("unmatched '('")
	ErrUnmatchedRpar       = Error("unmatched ')'")
	ErrUnmatchedLbkt       = Error("unmatched '['")
	ErrUnmatchedRbkt       = Error("unmatched ']'")
	ErrBadRange            = Error("bad range in character class")
	ErrExtraneousBackslash = Error("extraneous backslash")
	ErrBareClosure         = Error("closure applies to nothing")
	ErrBadBackslash        = Error("illegal backslash escape")
	ErrExpectedLBrace      = Error("expected '{'")
	ErrUnmatchedLBrace     = Error("unmatched '{'")
	ErrUnexpectedEOF       = Error("unexpected EOF")
	ErrUnexpectedNewline   = Error("unexpected newline")
	ErrUnmatchedLAngle     = Error("unmatched '<'")
	ErrUnmatchedRAngle     = Error("unmatched '>'")
)

func ispunct(c rune) bool {
	for _, r := range "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~" {
		if c == r {
			return true
		}
	}
	return false
}

var escapes = []rune("abfnrtv")
var escaped = []rune("\a\b\f\n\r\t\v")

func escape(c rune) rune {
	for i, b := range escapes {
		if b == c {
			return escaped[i]
		}
	}
	return -1
}

const (
	kNil = iota
	kRune
	kClass
	kWild
)

type edge struct {
	kind   int    // Rune/Class/Wild/Nil.
	r      rune   // Rune for rune edges.
	lim    []rune // Pairs of limits for character class edges.
	negate bool   // True if the character class is negated.
	dst    *node  // Destination node.
}
type node struct {
	e      []*edge // Outedges.
	n      int     // Index number. Scoped to a family.
	accept bool    // True if this is an accepting state.
	set    []int   // The NFA nodes represented by a DFA node.
}

// Print a graph in DOT format given the start node.
//
//  $ dot -Tps input.dot -o output.ps
func writeDotGraph(outf *os.File, start *node, id string) {
	done := make(map[*node]bool)
	var show func(*node)
	show = func(u *node) {
		if u.accept {
			fmt.Fprintf(outf, "  %v[style=filled,color=green];\n", u.n)
		}
		done[u] = true
		for _, e := range u.e {
			// We use -1 to denote the dead end node in DFAs.
			if e.dst.n == -1 {
				continue
			}
			label := ""
			runeToDot := func(r rune) string {
				if strconv.IsPrint(r) {
					return fmt.Sprintf("%v", string(r))
				}
				return fmt.Sprintf("U+%X", int(r))
			}
			switch e.kind {
			case kRune:
				label = fmt.Sprintf("[label=%q]", runeToDot(e.r))
			case kWild:
				label = "[color=blue]"
			case kClass:
				label = "[label=\"["
				if e.negate {
					label += "^"
				}
				for i := 0; i < len(e.lim); i += 2 {
					label += runeToDot(e.lim[i])
					if e.lim[i] != e.lim[i+1] {
						label += "-" + runeToDot(e.lim[i+1])
					}
				}
				label += "]\"]"
			}
			fmt.Fprintf(outf, "  %v -> %v%v;\n", u.n, e.dst.n, label)
		}
		for _, e := range u.e {
			if !done[e.dst] {
				show(e.dst)
			}
		}
	}
	fmt.Fprintf(outf, "digraph %v {\n  0[shape=box];\n", id)
	show(start)
	fmt.Fprintln(outf, "}")
}

func inClass(r rune, lim []rune) bool {
	for i := 0; i < len(lim); i += 2 {
		if lim[i] <= r && r <= lim[i+1] {
			return true
		}
	}
	return false
}

var dfadot, nfadot *os.File

func gen(out io.Writer, x *rule) {
	s := x.regex
	// Regex -> NFA
	// We cannot have our alphabet be all Unicode characters. Instead,
	// we compute an alphabet for each regex:
	//
	//   1. Singles: we add single runes used in the regex: any rune not in a
	//   range. These are held in `sing`.
	//
	//   2. Ranges: entire ranges become elements of the alphabet. If ranges in
	//   the same expression overlap, we break them up into non-overlapping
	//   ranges. The generated code checks singles before ranges, so there's no
	//   need to break up a range if it contains a single. These are maintained
	//   in sorted order in `lim`.
	//
	//   3. Wild: we add an element representing all other runes.
	//
	// e.g. the alphabet of /[0-9]*[Ee][2-5]*/ is sing: { E, e },
	// lim: { [0-1], [2-5], [6-9] } and the wild element.
	sing := make(map[rune]bool)
	var lim []rune
	var insertLimits func(l, r rune)
	// Insert a new range [l-r] into `lim`, breaking it up if it overlaps, and
	// discarding it if it coincides with an existing range. We keep `lim`
	// sorted.
	insertLimits = func(l, r rune) {
		var i int
		for i = 0; i < len(lim); i += 2 {
			if l <= lim[i+1] {
				break
			}
		}
		if len(lim) == i || r < lim[i] {
			lim = append(lim, 0, 0)
			copy(lim[i+2:], lim[i:])
			lim[i] = l
			lim[i+1] = r
			return
		}
		if l < lim[i] {
			lim = append(lim, 0, 0)
			copy(lim[i+2:], lim[i:])
			lim[i+1] = lim[i] - 1
			lim[i] = l
			insertLimits(lim[i], r)
			return
		}
		if l > lim[i] {
			lim = append(lim, 0, 0)
			copy(lim[i+2:], lim[i:])
			lim[i+1] = l - 1
			lim[i+2] = l
			insertLimits(l, r)
			return
		}
		// l == lim[i]
		if r == lim[i+1] {
			return
		}
		if r < lim[i+1] {
			lim = append(lim, 0, 0)
			copy(lim[i+2:], lim[i:])
			lim[i] = l
			lim[i+1] = r
			lim[i+2] = r + 1
			return
		}
		insertLimits(lim[i+1]+1, r)
	}
	pos := 0
	n := 0
	newNode := func() *node {
		res := new(node)
		res.n = n
		n++
		return res
	}
	newEdge := func(u, v *node) *edge {
		res := new(edge)
		res.dst = v
		u.e = append(u.e, res)
		return res
	}
	newWildEdge := func(u, v *node) *edge {
		res := newEdge(u, v)
		res.kind = kWild
		return res
	}
	newRuneEdge := func(u, v *node, r rune) *edge {
		res := newEdge(u, v)
		res.kind = kRune
		res.r = r
		sing[r] = true
		return res
	}
	newNilEdge := func(u, v *node) *edge {
		res := newEdge(u, v)
		res.kind = kNil
		return res
	}
	newClassEdge := func(u, v *node) *edge {
		res := newEdge(u, v)
		res.kind = kClass
		res.lim = make([]rune, 0, 2)
		return res
	}
	nlpar := 0
	maybeEscape := func() rune {
		c := s[pos]
		if '\\' == c {
			pos++
			if len(s) == pos {
				panic(ErrExtraneousBackslash)
			}
			c = s[pos]
			switch {
			case ispunct(c):
			case escape(c) >= 0:
				c = escape(s[pos])
			default:
				panic(ErrBadBackslash)
			}
		}
		return c
	}
	pcharclass := func() (start, end *node) {
		start, end = newNode(), newNode()
		e := newClassEdge(start, end)
		// Ranges consisting of a single element are a special case:
		singletonRange := func(c rune) {
			// 1. The edge-specific 'lim' field always expects endpoints in pairs,
			// so we must give 'c' as the beginning and the end of the range.
			e.lim = append(e.lim, c, c)
			// 2. Instead of updating the regex-wide 'lim' interval set, we add a singleton.
			sing[c] = true
		}
		if len(s) > pos && '^' == s[pos] {
			e.negate = true
			pos++
		}
		var left rune
		leftLive := false
		justSawDash := false
		first := true
		// Allow '-' at the beginning and end, and in ranges.
		for pos < len(s) && s[pos] != ']' {
			switch c := maybeEscape(); c {
			case '-':
				if first {
					singletonRange('-')
					break
				}
				justSawDash = true
			default:
				if justSawDash {
					if !leftLive || left > c {
						panic(ErrBadRange)
					}
					e.lim = append(e.lim, left, c)
					if left == c {
						sing[c] = true
					} else {
						insertLimits(left, c)
					}
					leftLive = false
				} else {
					if leftLive {
						singletonRange(left)
					}
					left = c
					leftLive = true
				}
				justSawDash = false
			}
			first = false
			pos++
		}
		if leftLive {
			singletonRange(left)
		}
		if justSawDash {
			singletonRange('-')
		}
		return
	}
	var pre func() (start, end *node)
	pterm := func() (start, end *node) {
		if len(s) == pos || s[pos] == '|' {
			end = newNode()
			start = end
			return
		}
		switch s[pos] {
		case '*', '+', '?':
			panic(ErrBareClosure)
		case ')':
			if 0 == nlpar {
				panic(ErrUnmatchedRpar)
			}
			end = newNode()
			start = end
			return
		case '(':
			nlpar++
			pos++
			start, end = pre()
			if len(s) == pos || ')' != s[pos] {
				panic(ErrUnmatchedLpar)
			}
		case '.':
			start, end = newNode(), newNode()
			newWildEdge(start, end)
		case ']':
			panic(ErrUnmatchedRbkt)
		case '[':
			pos++
			start, end = pcharclass()
			if len(s) == pos || ']' != s[pos] {
				panic(ErrUnmatchedLbkt)
			}
		default:
			start, end = newNode(), newNode()
			newRuneEdge(start, end, maybeEscape())
		}
		pos++
		return
	}
	pclosure := func() (start, end *node) {
		start, end = pterm()
		if start == end {
			return
		}
		if len(s) == pos {
			return
		}
		switch s[pos] {
		case '*':
			newNilEdge(end, start)
			nend := newNode()
			newNilEdge(end, nend)
			start, end = end, nend
		case '+':
			newNilEdge(end, start)
			nend := newNode()
			newNilEdge(end, nend)
			end = nend
		case '?':
			newNilEdge(start, end)
		default:
			return
		}
		pos++
		return
	}
	pcat := func() (start, end *node) {
		for {
			nstart, nend := pclosure()
			if start == nil {
				start, end = nstart, nend
			} else if nstart != nend {
				end.e = make([]*edge, len(nstart.e))
				copy(end.e, nstart.e)
				end = nend
			}
			if nstart == nend {
				return
			}
		}
		panic("unreachable")
	}
	pre = func() (start, end *node) {
		start, end = pcat()
		for pos < len(s) {
			if s[pos] != '|' {
				panic(ErrInternal)
			}
			pos++
			nstart, nend := pcat()
			tmp := newNode()
			newNilEdge(tmp, start)
			newNilEdge(tmp, nstart)
			start = tmp
			tmp = newNode()
			newNilEdge(end, tmp)
			newNilEdge(nend, tmp)
			end = tmp
		}
		return
	}
	start, end := pre()
	end.accept = true

	// Compute shortlist of nodes (reachable nodes), as we may have discarded
	// nodes left over from parsing. Also, make short[0] the start node.
	short := make([]*node, 0, n)
	{
		var visit func(*node)
		mark := make([]bool, n)
		newn := make([]int, n)
		visit = func(u *node) {
			mark[u.n] = true
			newn[u.n] = len(short)
			short = append(short, u)
			for _, e := range u.e {
				if !mark[e.dst.n] {
					visit(e.dst)
				}
			}
		}
		visit(start)
		for _, v := range short {
			v.n = newn[v.n]
		}
	}
	n = len(short)

	if nfadot != nil {
		writeDotGraph(nfadot, start, fmt.Sprintf("NFA_%v", x.index))
	}

	// NFA -> DFA
	nilClose := func(st []bool) {
		mark := make([]bool, n)
		var do func(int)
		do = func(i int) {
			v := short[i]
			for _, e := range v.e {
				if e.kind == kNil && !mark[e.dst.n] {
					st[e.dst.n] = true
					do(e.dst.n)
				}
			}
		}
		for i := 0; i < n; i++ {
			if st[i] && !mark[i] {
				mark[i] = true
				do(i)
			}
		}
	}
	var todo []*node
	tab := make(map[string]*node)
	var buf []byte
	dfacount := 0
	{ // Construct the node of no return.
		for i := 0; i < n; i++ {
			buf = append(buf, '0')
		}
		tmp := new(node)
		tmp.n = -1
		tab[string(buf)] = tmp
	}
	newDFANode := func(st []bool) (res *node, found bool) {
		buf = nil
		accept := false
		for i, v := range st {
			if v {
				buf = append(buf, '1')
				accept = accept || short[i].accept
			} else {
				buf = append(buf, '0')
			}
		}
		res, found = tab[string(buf)]
		if !found {
			res = new(node)
			res.n = dfacount
			res.accept = accept
			dfacount++
			for i, v := range st {
				if v {
					res.set = append(res.set, i)
				}
			}
			tab[string(buf)] = res
		}
		return res, found
	}

	get := func(states []bool) *node {
		nilClose(states)
		node, old := newDFANode(states)
		if !old {
			todo = append(todo, node)
		}
		return node
	}
	states := make([]bool, n)
	// The DFA start state is the state representing the nil-closure of the start
	// node in the NFA. Recall it has index 0.
	states[0] = true
	dfastart := get(states)
	for len(todo) > 0 {
		v := todo[len(todo)-1]
		todo = todo[0 : len(todo)-1]
		// Singles.
		for r, _ := range sing {
			states := make([]bool, n)
			for _, i := range v.set {
				for _, e := range short[i].e {
					if e.kind == kRune && e.r == r {
						states[e.dst.n] = true
					} else if e.kind == kWild {
						states[e.dst.n] = true
					} else if e.kind == kClass && e.negate != inClass(r, e.lim) {
						states[e.dst.n] = true
					}
				}
			}
			newRuneEdge(v, get(states), r)
		}
		// Character ranges.
		for j := 0; j < len(lim); j += 2 {
			states := make([]bool, n)
			for _, i := range v.set {
				for _, e := range short[i].e {
					if e.kind == kWild {
						states[e.dst.n] = true
					} else if e.kind == kClass && e.negate != inClass(lim[j], e.lim) {
						states[e.dst.n] = true
					}
				}
			}
			e := newClassEdge(v, get(states))
			e.lim = append(e.lim, lim[j], lim[j+1])
		}
		// Wild.
		states := make([]bool, n)
		for _, i := range v.set {
			for _, e := range short[i].e {
				if e.kind == kWild || (e.kind == kClass && e.negate) {
					states[e.dst.n] = true
				}
			}
		}
		newWildEdge(v, get(states))
	}
	n = dfacount

	if dfadot != nil {
		writeDotGraph(dfadot, dfastart, fmt.Sprintf("DFA_%v", x.index))
	}
	// DFA -> Go
	// TODO: Literal arrays instead of a series of assignments.
	fmt.Fprintf(out, "{\nvar acc [%d]bool\nvar fun [%d]func(rune) int\n", n, n)
	for _, v := range tab {
		if -1 == v.n {
			continue
		}
		if v.accept {
			fmt.Fprintf(out, "acc[%d] = true\n", v.n)
		}
		fmt.Fprintf(out, "fun[%d] = func(r rune) int {\n", v.n)
		var runeCases, classCases string
		var wildDest int
		for _, e := range v.e {
			m := e.dst.n
			switch e.kind {
			case kRune:
				runeCases += fmt.Sprintf("\t\tcase %d: return %d\n", e.r, m)
			case kClass:
				classCases += fmt.Sprintf("\t\tcase %d <= r && r <= %d: return %d\n",
					e.lim[0], e.lim[1], m)
			case kWild:
				wildDest = m
			}
		}
		if runeCases != "" {
			fmt.Fprintf(out, "\tswitch(r) {\n"+runeCases+"\t}\n")
		}
		if classCases != "" {
			fmt.Fprintf(out, "\tswitch {\n"+classCases+"\t}\n")
		}
		fmt.Fprintf(out, "\treturn %v\n}\n", wildDest)
	}
}

var standalone, customError, autorun *bool

func writeLex(out *bufio.Writer, families [][]*rule) {
	if !*customError {
		out.WriteString(`func (yylex Lexer) Error(e string) {
  panic(e)
}`)
	}
	out.WriteString(`
func (yylex *Lexer) Lex(lval *yySymType) int {
`)
  for i, _ := range families {
    fmt.Fprintf(out, "var nex%d func(*Lexer) int\n", i)
  }
  for i, rules := range families {
  fmt.Fprintf(out, `
nex%d = func(c *Lexer) int {
  helpful: switch c.nextAction() {
`, i)
    for _, x := range rules {
      fmt.Fprintf(out, "\n\t\tcase %d:  //%s/\n", x.index, string(x.regex))
      out.WriteString(x.code)
      if x.index != -1 {
        out.WriteString("\n\t\tgoto helpful\n")
      }
    }
    out.WriteString("\n\t}\nreturn 0\n}\n")
  }
	out.WriteString("\t\treturn nex0(yylex)\n}\n")
}
func writeNNFun(out *bufio.Writer, families [][]*rule) {
	out.WriteString("func(yylex *Lexer) {\n")
  for i, _ := range families {
    fmt.Fprintf(out, "var nex%d func(*Lexer)\n", i)
  }
  for i, rules := range families {
  fmt.Fprintf(out, `
nex%d = func(c *Lexer) {
  helpful: switch c.nextAction() {
`, i)
    for _, x := range rules {
      fmt.Fprintf(out, "\n\t\tcase %d:  //%s/\n", x.index, string(x.regex))
      out.WriteString(x.code)
      if x.index != -1 {
        out.WriteString("\n\t\tgoto helpful\n")
      }
    }
    out.WriteString("\n\t}\n\n}\n")
  }
	out.WriteString("\t\tnex0(yylex)\n\t}")
}
func process(output io.Writer, input io.Reader) {
	in := bufio.NewReader(input)
	out := bufio.NewWriter(output)
	var r rune
	read := func() bool {
		var err error
		r, _, err = in.ReadRune()
		if err == io.EOF {
			return true
		}
		if err != nil {
			panic(err)
		}
		return false
	}
	skipws := func() bool {
		for !read() {
			if strings.IndexRune(" \n\t\r", r) == -1 {
				return false
			}
		}
		return true
	}
  var families [][]*rule
	usercode := false
	var buf []rune
	readCode := func() string {
		if '{' != r {
			panic(ErrExpectedLBrace)
		}
		buf = []rune{r}
		nesting := 1
		for {
			if read() {
				panic(ErrUnmatchedLBrace)
			}
			buf = append(buf, r)
			if '{' == r {
				nesting++
			} else if '}' == r {
				nesting--
				if 0 == nesting {
					break
				}
			}
		}
		return string(buf)
	}
	var decls string
	var parse func()
	parse = func() {
    family := len(families)
    families = append(families, nil)
		rulen := 0
		declvar := func() {
			decls += fmt.Sprintf("var a%d [%d]dfa\n", family, rulen)
		}
    newRule := func(index int) *rule {
      x := new(rule)
      families[family] = append(families[family], x)
      x.index = index
      return x
    }
		for {
			if skipws() {
        panic(ErrUnexpectedEOF)
			}
			if '>' == r {
				if 0 == family {
					panic(ErrUnmatchedRAngle)
				}
				x := newRule(-1)
				declvar()
				if skipws() {
					panic(ErrUnexpectedEOF)
				}
				x.code += readCode()
				return
			}
			delim := r
			if read() {
				panic(ErrUnexpectedEOF)
			}
      var regex []rune
			for {
				if r == delim && (len(regex) == 0 || regex[len(regex)-1] != '\\') {
					break
				}
				if '\n' == r {
					panic(ErrUnexpectedNewline)
				}
				regex = append(regex, r)
				if read() {
					panic(ErrUnexpectedEOF)
				}
			}
			if "" == string(regex) {
				usercode = true
				break
			}
			if skipws() {
				panic("last pattern lacks action")
			}
			x := newRule(rulen)
			rulen++
			x.regex = make([]rune, len(regex))
			copy(x.regex, regex)
			nested := false
			if '<' == r {
				if skipws() {
					panic("'<' lacks action")
				}
				nested = true
			}
			x.code += readCode()
			if nested {
				x.code += fmt.Sprintf("\nnex%d(newFrame(bufio.NewReader(strings.NewReader(c.text)), %d))\n", len(families), len(families))
				parse()
			}
		}
		if 0 != family {
			panic(ErrUnmatchedLAngle)
		}
		x := newRule(-1)
		x.code = "// [END]\n"
		declvar()
	}
	parse()

	if !usercode {
		return
	}

	buf = nil
	for done := skipws(); !done; done = read() {
		buf = append(buf, r)
	}
	fs := token.NewFileSet()
	t, err := parser.ParseFile(fs, "", string(buf), parser.ImportsOnly)
	if err != nil {
		panic(err)
	}
	printer.Fprint(out, fs, t)

	var file *token.File
	fs.Iterate(func(f *token.File) bool {
		file = f
		return true
	})

	for m := file.LineCount(); m > 1; m-- {
		i := 0
		for '\n' != buf[i] {
			i++
		}
		buf = buf[i+1:]
	}

	out.WriteString(`import ("bufio";"io";"strings")
type dfa struct {
  acc []bool  // Accepting states.
  f []func(rune) int  // Transitions.
}
type family struct {
  a []dfa
}
` + decls)
  fmt.Fprintf(out, `var a [%d]family
func init() {
`, len(families))
  for family, rules := range families {
    for i, x := range rules {
      if -1 == x.index {
        continue
      }
      gen(out, x)
      fmt.Fprintf(out, "a%d[%d].acc = acc[:]\n", family, i)
      fmt.Fprintf(out, "a%d[%d].f = fun[:]\n", family, i)
      fmt.Fprintf(out, "}\n")
    }
  }
	for i := 0; i < len(families); i++ {
		fmt.Fprintf(out, "a[%d].a = a%d[:]\n", i, i)
	}

	out.WriteString(`}
type Lexer struct {
  atEOF bool
  buf []rune
  text string
  in *bufio.Reader
  fam family
  n int
}
func newFrame(in *bufio.Reader, index int) *Lexer {
  f := new(Lexer)
  f.in = in
  f.fam = a[index]
  return f
}
func newFrameFromString(s string, family int) *Lexer {
  return newFrame(bufio.NewReader(strings.NewReader(s)), family)
}
func NewLexer(in io.Reader) *Lexer {
  return newFrame(bufio.NewReader(in), 0)
}
func (c *Lexer) nextAction() int {
  matchi := 0  // Index of DFA with highest-precedence match so far.
  matchn := 0  // Length of highest-precedence match so far.
  state := make([]int, len(c.fam.a))
  for {
    if c.atEOF { return -1 }
    if c.n == len(c.buf) {
      r,_,err := c.in.ReadRune()
      switch err {
      case io.EOF: c.atEOF = true
      case nil:    c.buf = append(c.buf, r)
      default:     panic(err)
      }
    }
    jammed := true
    if !c.atEOF {
      r := c.buf[c.n]
      c.n++
      for i, x := range c.fam.a {
        if -1 == state[i] { continue }
        state[i] = x.f[state[i]](r)
        if -1 == state[i] { continue }
        jammed = false
        // Higher precedence match? DFAs are run in parallel, so matchn is at most len(c.buf), hence we may omit the length equality check.
        if x.acc[state[i]] && (matchn < c.n || matchi > i) {
          matchi = i
          matchn = c.n
        }
      }
    }

    if jammed {
      // All DFAs stuck. Return last match if it exists, otherwise advance by one rune and restart all DFAs.
      c.n = 0
      if matchn == 0 {
        if c.atEOF {  // Empty input.
          return -1
        }
        state = make([]int, len(c.fam.a))
        c.buf = c.buf[1:]
      } else {
        c.text = string(c.buf[:matchn])
        c.buf = c.buf[matchn:]
        matchn = 0
        return matchi
      }
    }
  }
  panic("unreachable")
}
func (yylex *Lexer) Text() string {
  return yylex.text
}
`)
	if !*standalone {
		writeLex(out, families)
		out.WriteString(string(buf))
		out.Flush()
		return
	}
	m := 0
	const funmac = "NN_FUN"
	for m < len(buf) {
		m++
		if funmac[:m] != string(buf[:m]) {
			out.WriteString(string(buf[:m]))
			buf = buf[m:]
			m = 0
		} else if funmac == string(buf[:m]) {
			writeNNFun(out, families)
			buf = buf[m:]
			m = 0
		}
	}
	out.WriteString(string(buf))
	out.Flush()
}

func dieIf(cond bool, v ...interface{}) {
	if cond {
		fmt.Println(v...)
		os.Exit(1)
	}
}

func dieErr(err error, s string) {
	if err != nil {
		fmt.Printf("%v: %v", s, err)
		os.Exit(1)
	}
}

func createDotFile(filename string) *os.File {
	if filename == "" {
		return nil
	}
	dieIf(strings.HasSuffix(filename, ".nex"), "nex: DOT filename ends with .nex:", filename)
	file, err := os.Create(filename)
	dieErr(err, "Create")
	return file
}

func main() {
	standalone = flag.Bool("s", false, `standalone code; NN_FUN macro substitution, no Lex() method`)
	customError = flag.Bool("e", false, `custom error func; no Error() method`)
	autorun = flag.Bool("r", false, `run generated program`)
	nfadotFile := flag.String("nfadot", "", `show NFA graph in DOT format`)
	dfadotFile := flag.String("dfadot", "", `show DFA graph in DOT format`)
	flag.Parse()

	nfadot = createDotFile(*nfadotFile)
	dfadot = createDotFile(*dfadotFile)
	defer func() {
		if nfadot != nil {
			dieErr(nfadot.Close(), "Close")
		}
		if dfadot != nil {
			dieErr(dfadot.Close(), "Close")
		}
	}()
	infile, outfile := os.Stdin, os.Stdout
	var err error
	if flag.NArg() > 0 {
		dieIf(flag.NArg() > 1, "nex: extraneous arguments after", flag.Arg(0))
		dieIf(strings.HasSuffix(flag.Arg(0), ".go"), "nex: input filename ends with .go:", flag.Arg(0))
		basename := flag.Arg(0)
		n := strings.LastIndex(basename, ".")
		if n >= 0 {
			basename = basename[:n]
		}
		infile, err = os.Open(flag.Arg(0))
		dieErr(err, "nex")
		defer infile.Close()
		if !*autorun {
			outfile, err = os.Create(basename + ".nn.go")
			dieErr(err, "nex")
			defer outfile.Close()
		}
	}
	if *autorun {
		tmpdir, err := ioutil.TempDir("", "nex")
		dieIf(err != nil, "tempdir:", err)
		defer func() {
			dieErr(os.RemoveAll(tmpdir), "RemoveAll")
		}()
		outfile, err = os.Create(tmpdir + "/lets.go")
		dieErr(err, "nex")
		defer outfile.Close()
	}
	process(outfile, infile)
	if *autorun {
		c := exec.Command("go", "run", outfile.Name())
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		dieErr(c.Run(), "go run")
	}
}
