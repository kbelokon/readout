package yamlview

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// tokenClass maps a chroma YAML TokenType onto the Pygments `.highlight .X`
// class string that the baked readout.css palette colours. Only the token
// types the YAML lexer (chroma v2.26.1 embedded/yaml.xml) can emit are listed;
// resolution falls back through the type's sub-category then category so that
// e.g. LiteralNumberInteger resolves via LiteralNumber. A token type with no
// entry (Text, Other, Ignore, ...) renders as a raw escaped string with no
// wrapping span -- matching Pygments' convention that Token.Text carries no
// class. Every class here exists in readout.css's 290 `.highlight .X` rules,
// so no CSS edit is required (these map onto the existing classes).
var tokenClass = map[chroma.TokenType]string{
	chroma.NameTag:             "nt", // mapping keys
	chroma.NameNamespace:       "nn", // --- / ... document markers
	chroma.Punctuation:         "p",  // : , [ ] | > and block-scalar indicators
	chroma.TextWhitespace:      "w",  // indentation / inter-token spaces
	chroma.Literal:             "l",  // plain (unquoted) scalars
	chroma.LiteralDate:         "ld", // bare timestamps
	chroma.LiteralString:       "s",  // string base
	chroma.LiteralStringDouble: "s2", // "double quoted"
	chroma.LiteralStringSingle: "s1", // 'single quoted'
	chroma.LiteralStringDoc:    "sd", // block-scalar body lines
	chroma.LiteralNumber:       "m",  // numbers
	chroma.KeywordConstant:     "kc", // true/false/null/yes/no/on/off
	chroma.Comment:             "c",  // # comments
	chroma.CommentPreproc:      "cp", // !!tags / &anchors / *aliases
}

// classFor resolves the Pygments class for a token type, walking the chroma
// type hierarchy (exact -> sub-category -> category) so unlisted leaf types
// inherit their family's class. Returns "" when the token should be emitted raw.
func classFor(t chroma.TokenType) string {
	if c, ok := tokenClass[t]; ok {
		return c
	}
	if c, ok := tokenClass[t.SubCategory()]; ok {
		return c
	}
	if c, ok := tokenClass[t.Category()]; ok {
		return c
	}
	return ""
}

// Highlight renders yaml as the Pygments-compatible highlight table that the
// resource-view frontend depends on. chroma is used ONLY as the lexer/tokeniser
// (lexers.Get("yaml")); a custom classes-only formatter walks the per-line token
// slices and emits this exact DOM:
//
//	<div class="highlight"><table class="highlighttable"><tr>
//	  <td class="linenos"><div class="linenodiv"><pre>
//	    <span class="normal"><a href="#<prefix>line-N"> N</a></span> ...
//	  </pre></div></td>
//	  <td class="code"><div><pre><span></span>
//	    <span id="yaml-<prefix>line-N"><a id="<prefix>line-N" name="<prefix>line-N"></a>...line...
//	</span> ...
//	  </pre></div></td>
//	</tr></table></div>
//
// The dual id scheme is preserved: the gutter <a> targets #<prefix>line-N (the
// inner bare anchor) while the code-cell line span carries id="yaml-<prefix>line-N".
// readout.js (deep-link/highlight/buildYamlFolds/copy) hard-codes this exact
// structure; it is pinned by TestBehaviorPodYAMLViewIDScheme.
//
// anchorPrefix is "" for the full YAML view and "<key>-" for a per-key section
// card (e.g. "metadata-"). linkTimestamps, when non-nil, transforms each rendered
// code line (used to inject timestamp <a> links); pass nil for no transform. It
// is applied to the rendered line HTML exactly as the previous emitter did, so
// the regex-over-rendered-text timestamp linking behaves identically.
func Highlight(yaml, anchorPrefix string, linkTimestamps func(line string) string) string {
	yaml = strings.TrimSuffix(yaml, "\n")
	rendered := renderLines(yaml)

	var b strings.Builder
	b.WriteString(`<div class="highlight"><table class="highlighttable"><tr><td class="linenos"><div class="linenodiv"><pre>`)
	width := len(strconv.Itoa(len(rendered)))
	for i := range rendered {
		lineNo := i + 1
		fmt.Fprintf(&b, `<span class="normal"><a href="#%sline-%d">%*d</a></span>`, html.EscapeString(anchorPrefix), lineNo, width, lineNo)
		if i < len(rendered)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteString(`</pre></div></td><td class="code"><div><pre><span></span>`)
	for i, line := range rendered {
		lineNo := i + 1
		if linkTimestamps != nil {
			line = linkTimestamps(line)
		}
		fmt.Fprintf(&b, `<span id="yaml-%sline-%d"><a id="%sline-%d" name="%sline-%d"></a>%s`,
			html.EscapeString(anchorPrefix), lineNo,
			html.EscapeString(anchorPrefix), lineNo,
			html.EscapeString(anchorPrefix), lineNo, line)
		b.WriteByte('\n')
		b.WriteString(`</span>`)
	}
	b.WriteString(`</pre></div></td></tr></table></div>`)
	return b.String()
}

// renderLines tokenises yaml with chroma's YAML lexer and returns one rendered
// HTML fragment per source line (newline stripped; it is re-emitted by Highlight
// between the line content and the closing </span>, matching the old DOM where
// the trailing newline lives INSIDE the line span -- buildYamlFolds reads each
// line span's textContent including that newline). On a lexer error the input is
// rendered as plain escaped lines (graceful, still well-formed line spans).
func renderLines(yaml string) []string {
	lexer := lexers.Get("yaml")
	if lexer == nil {
		return escapedLines(yaml)
	}
	tokens, err := chroma.Tokenise(lexer, nil, yaml)
	if err != nil {
		return escapedLines(yaml)
	}
	// Drive line boundaries off the newline characters in the token stream so the
	// rendered line set is byte-faithful to the source: exactly one line per
	// source line (count('\n')+1, identical to strings.Split(yaml,"\n")), with
	// every character preserved in order. A token spanning several lines (a block
	// scalar) is split at each '\n' and the same class is applied to each segment.
	// (chroma.SplitTokensIntoLines is avoided here: it drops a trailing blank line
	// for keep-chomped |+ block scalars, which would desync the gutter, the copy
	// textContent, and the per-line ids from the raw YAML.)
	var out []string
	var b strings.Builder
	emit := func(cls, val string) {
		if val == "" {
			return
		}
		if cls != "" {
			b.WriteString(`<span class="`)
			b.WriteString(cls)
			b.WriteString(`">`)
			b.WriteString(html.EscapeString(val))
			b.WriteString(`</span>`)
		} else {
			b.WriteString(html.EscapeString(val))
		}
	}
	for _, tok := range tokens {
		cls := classFor(tok.Type)
		val := tok.Value
		for {
			nl := strings.IndexByte(val, '\n')
			if nl < 0 {
				emit(cls, val)
				break
			}
			emit(cls, val[:nl])
			out = append(out, b.String())
			b.Reset()
			val = val[nl+1:]
		}
	}
	out = append(out, b.String())
	return out
}

func escapedLines(yaml string) []string {
	lines := strings.Split(yaml, "\n")
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = html.EscapeString(l)
	}
	return out
}
