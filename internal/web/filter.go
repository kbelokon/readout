package web

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/kbelokon/readout/internal/kube"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// filter.go is the server half of Filters v2 (design D7): parsing the
// repeatable `?f=` query params into chips and matching table rows against
// them. The chip grammar is pinned:
//
//   - a chip is `field OP value`, OP ∈ {`:`, `!=`, `>`, `<`}. `:` is a
//     case-insensitive SUBSTRING match (status:Run matches Running), `!=` its
//     negation. The FIRST operator occurrence splits field from value, so
//     values containing `:` or `=` survive (label:app.kubernetes.io/name=api
//     -> field "label", value "app.kubernetes.io/name=api").
//   - `,` in the value is OR. Alternatives split on RAW unencoded commas
//     BEFORE percent-decoding each alternative, so an encoded `%2C` is a
//     literal comma INSIDE one alternative (typed input treats every comma as
//     OR; deep links can encode). This is why parsing starts from RawQuery,
//     never from url.Values (which pre-decodes %2C into an indistinguishable
//     comma).
//   - chips AND-combine; the URL form is repeatable `?f=<chip>`.
//   - field names resolve against the Table's columns case-insensitively;
//     multi-word columns match with spaces or dashes (`nominated node` /
//     `nominated-node`). An unknown field matches ZERO rows (the client
//     prevents the chip; the server stays strict and silent). A malformed
//     chip with no operator is unmatchable for the same reason: it cannot be
//     evaluated, and silently showing all rows would lie about the filter
//     being applied.
//   - `cpu` / `memory` are reserved ALIASES for the joined "CPU Usage" /
//     "Memory Usage" metrics columns when present; they never bind to the
//     nodes' plain "CPU"/"Memory" CAPACITY columns. With no metrics join the
//     alias is an unknown field (zero rows). `>`/`<` on the alias converts
//     the RHS via resource.ParseQuantity (500m -> 0.5 cores, 100Mi -> bytes)
//     against the joined float cells.
//   - `>`/`<` elsewhere compare as durations when the RHS is a kubectl-age
//     token (59s, 5m33s, 2d3h -- HumanDuration emits one- AND two-unit
//     tokens), else numerically when the RHS is a number. Cells that do not
//     parse in the chosen mode are EXCLUDED from `>`/`<` matches. Decorated
//     cells parse their leading token (restarts "3 (4m ago)" -> 3, with
//     thousands separators stripped: "1,047" -> 1047).
//   - `label:key=value` matches the row object's metadata.labels: the key
//     exactly, the value as a case-insensitive substring (the `:` operator's
//     meaning); `label:key` alone matches key existence. `>`/`<` on labels
//     match nothing.
//
// The legacy params (`?filter`/`?selector`/`?hidecols`/`?labelcols`) are
// untouched and AND-combine with `f` in the same pipeline.

// filterOp is one pinned chip operator.
type filterOp string

const (
	opContains    filterOp = ":"
	opNotContains filterOp = "!="
	opGreater     filterOp = ">"
	opLess        filterOp = "<"
)

// filterChip is one parsed `?f=` chip. Field/Values hold decoded text; Raw
// keeps the still-encoded query value so a chip-removal href can drop exactly
// this occurrence without re-encoding (and thus re-interpreting) its siblings.
// An Op of "" marks a malformed chip (no operator found): it is kept so the
// empty-filtered state can still display and remove it, but it matches no row.
type filterChip struct {
	Field  string
	Op     filterOp
	Values []string
	Raw    string
}

// display is the human form of the chip for the empty-filtered state chips:
// the decoded expression as typed (field, operator, comma-joined values).
func (c filterChip) display() string {
	if c.Op == "" {
		return strings.Join(c.Values, ",")
	}
	return c.Field + string(c.Op) + strings.Join(c.Values, ",")
}

// parseFilterParams extracts every `f=` chip from a RAW query string. It must
// receive RawQuery (not url.Values): the OR-comma split is defined on the raw
// text, where `,` and `%2C` are still distinguishable.
func parseFilterParams(rawQuery string) []filterChip {
	if !strings.Contains(rawQuery, "f=") {
		// Fast path. Every real `f=` pair contains the literal "f=", so this
		// check can skip work but never skip a chip.
		return nil
	}
	var chips []filterChip
	for _, pair := range strings.Split(rawQuery, "&") {
		key, raw, found := strings.Cut(pair, "=")
		if !found || key != "f" || raw == "" {
			continue
		}
		chips = append(chips, parseFilterChip(raw))
	}
	return chips
}

// parseFilterChip parses one raw (still-encoded) chip value. Order is load-
// bearing: (1) split OR alternatives on RAW commas, (2) percent-decode each
// alternative (so %2C survives as a literal comma inside one alternative),
// (3) split field/operator on the FIRST operator occurrence in the decoded
// first segment (so encoded operators like %3C work, and `=`/`:` inside the
// value survive).
func parseFilterChip(raw string) filterChip {
	segments := strings.Split(raw, ",")
	decoded := make([]string, len(segments))
	for i, seg := range segments {
		decoded[i] = decodeQueryComponent(seg)
	}
	field, op, value := splitFilterOperator(decoded[0])
	chip := filterChip{Raw: raw, Op: op}
	if op == "" {
		chip.Values = []string{strings.TrimSpace(decoded[0])}
		return chip
	}
	chip.Field = strings.TrimSpace(field)
	values := make([]string, 0, len(decoded))
	values = append(values, strings.TrimSpace(value))
	for _, alt := range decoded[1:] {
		values = append(values, strings.TrimSpace(alt))
	}
	// Drop empty alternatives: a trailing/doubled comma must not turn the chip
	// into match-everything. When ALL alternatives are empty keep one empty
	// value, so `f=status:` stays a well-defined match-all no-op.
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 {
		kept = []string{""}
	}
	chip.Values = kept
	return chip
}

// splitFilterOperator finds the FIRST operator occurrence scanning left to
// right and splits the chip there. `!` and `=` alone are not operators, so
// `label:a=b` splits on the `:` and the `=` stays in the value. Returns op ""
// when the text contains no operator. Byte scanning is UTF-8 safe here: all
// operator bytes are ASCII and never occur inside multi-byte sequences.
func splitFilterOperator(s string) (string, filterOp, string) {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '!' && i+1 < len(s) && s[i+1] == '=':
			return s[:i], opNotContains, s[i+2:]
		case s[i] == ':':
			return s[:i], opContains, s[i+1:]
		case s[i] == '>':
			return s[:i], opGreater, s[i+1:]
		case s[i] == '<':
			return s[:i], opLess, s[i+1:]
		}
	}
	return "", "", s
}

// decodeQueryComponent percent-decodes one already-comma-split alternative
// (`+` means space in query strings). Malformed escapes degrade to the raw
// text instead of dropping the chip -- strict zero-row matching then applies
// if the text resolves to no field.
func decodeQueryComponent(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

// applyFilterChips removes the rows that do not match EVERY chip (chips AND;
// values within a chip OR). It runs on the full dataset before sort/limit.
func applyFilterChips(table *kube.Table, chips []filterChip) {
	if len(chips) == 0 {
		return
	}
	matchers := make([]rowMatcher, len(chips))
	for i := range chips {
		matchers[i] = chips[i].matcher(table)
	}
	filtered := table.Rows[:0]
	for _, row := range table.Rows {
		keep := true
		for _, match := range matchers {
			if !match(row) {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
}

// rowMatcher reports whether one row passes one chip.
type rowMatcher func(row kube.Row) bool

// neverMatch is the strict zero-row matcher for unknown fields and malformed
// chips: the filter cannot be evaluated, so no row may claim to pass it.
func neverMatch(kube.Row) bool { return false }

// matcher resolves the chip's field against THIS table's columns and returns
// the row predicate. Resolution order: the `label` virtual field, then the
// reserved cpu/memory metrics aliases, then the table columns.
func (c filterChip) matcher(table *kube.Table) rowMatcher {
	if c.Op == "" {
		return neverMatch
	}
	if strings.EqualFold(c.Field, "label") {
		return labelMatcher(c.Op, c.Values)
	}
	if alias, ok := filterFieldAlias(c.Field); ok {
		// The alias binds ONLY to the joined metrics column. When the metrics
		// join is absent the chip is an unknown field (zero rows) -- it must
		// never fall through to the nodes' "CPU"/"Memory" CAPACITY columns,
		// which the bare names would otherwise resolve to case-insensitively.
		idx := columnIndex(table.Columns, alias)
		if idx < 0 {
			return neverMatch
		}
		if c.Op == opGreater || c.Op == opLess {
			return quantityMatcher(idx, c.Op, c.Values)
		}
		return substringMatcher(idx, c.Op, c.Values)
	}
	idx := resolveFilterColumn(table.Columns, c.Field)
	if idx < 0 {
		return neverMatch
	}
	if c.Op == opGreater || c.Op == opLess {
		return compareMatcher(idx, c.Op, c.Values)
	}
	return substringMatcher(idx, c.Op, c.Values)
}

// filterFieldAlias maps the reserved metrics filter fields to the joined
// usage column names (build_list.go applyMetricsUsage appends exactly these).
func filterFieldAlias(field string) (string, bool) {
	switch {
	case strings.EqualFold(field, "cpu"):
		return "CPU Usage", true
	case strings.EqualFold(field, "memory"):
		return "Memory Usage", true
	}
	return "", false
}

// resolveFilterColumn resolves a field name against the table columns: exact
// case-insensitive first, then dash/space-normalized so multi-word columns
// match both ways (`nominated node` / `nominated-node` -> "Nominated Node",
// while "External-IP" still matches `external-ip` exactly).
func resolveFilterColumn(cols []kube.Column, field string) int {
	for i := range cols {
		if strings.EqualFold(cols[i].Name, field) {
			return i
		}
	}
	want := normalizeFieldName(field)
	for i := range cols {
		if normalizeFieldName(cols[i].Name) == want {
			return i
		}
	}
	return -1
}

func normalizeFieldName(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", " "))
}

// substringMatcher implements `:` (any alternative is a case-insensitive
// substring of the cell) and `!=` (its negation).
func substringMatcher(idx int, op filterOp, values []string) rowMatcher {
	lowered := make([]string, len(values))
	for i, v := range values {
		lowered[i] = strings.ToLower(v)
	}
	return func(row kube.Row) bool {
		cell := strings.ToLower(cellDisplayString(cellAt(row, idx)))
		matched := false
		for _, v := range lowered {
			if strings.Contains(cell, v) {
				matched = true
				break
			}
		}
		if op == opNotContains {
			return !matched
		}
		return matched
	}
}

// compareAlt is one precompiled `>`/`<` alternative: its threshold and
// whether it compares as a duration (RHS was a kubectl-age token) or a plain
// number. Alternatives that parse as neither are dropped -- they can never
// match.
type compareAlt struct {
	duration bool
	rhs      float64
}

func compareAlternatives(values []string) []compareAlt {
	var alts []compareAlt
	for _, v := range values {
		if seconds, ok := parseAgeToken(v); ok {
			alts = append(alts, compareAlt{duration: true, rhs: seconds})
			continue
		}
		if f, ok := parseFilterNumber(v); ok {
			alts = append(alts, compareAlt{rhs: f})
		}
	}
	return alts
}

// compareMatcher implements `>`/`<` on regular columns. The mode is chosen
// per alternative by its RHS shape: a kubectl-age token compares as a
// duration (the cell's leading age token; do NOT confuse with ageClass, which
// parses RFC3339 timestamps, not tokens), anything numeric compares as a
// number (the cell's leading numeric token, commas stripped). Cells that do
// not parse in the chosen mode are excluded from the match.
func compareMatcher(idx int, op filterOp, values []string) rowMatcher {
	alts := compareAlternatives(values)
	return func(row kube.Row) bool {
		cell := cellAt(row, idx)
		text := cellDisplayString(cell)
		for _, alt := range alts {
			var lhs float64
			var ok bool
			if alt.duration {
				lhs, ok = parseCellAgeToken(text)
			} else {
				lhs, ok = cellNumber(cell)
			}
			if !ok {
				continue
			}
			if compareFloats(lhs, op, alt.rhs) {
				return true
			}
		}
		return false
	}
}

// quantityMatcher implements `>`/`<` on the cpu/memory metrics aliases: the
// RHS converts via resource.ParseQuantity (500m -> 0.5 cores, 100Mi -> bytes)
// and compares against the joined float cells.
func quantityMatcher(idx int, op filterOp, values []string) rowMatcher {
	var rhs []float64
	for _, v := range values {
		if q, err := resource.ParseQuantity(v); err == nil {
			rhs = append(rhs, q.AsApproximateFloat64())
		}
	}
	return func(row kube.Row) bool {
		lhs, ok := numericCell(cellAt(row, idx))
		if !ok {
			return false
		}
		for _, r := range rhs {
			if compareFloats(lhs, op, r) {
				return true
			}
		}
		return false
	}
}

func compareFloats(lhs float64, op filterOp, rhs float64) bool {
	if op == opGreater {
		return lhs > rhs
	}
	return lhs < rhs
}

// labelMatcher implements the `label` virtual field against the row object's
// metadata.labels. Each alternative is `key=value` (key exact, value a
// case-insensitive substring -- the `:` operator's meaning) or a bare `key`
// (existence). `!=` negates the whole OR; `>`/`<` on labels match nothing.
func labelMatcher(op filterOp, values []string) rowMatcher {
	if op == opGreater || op == opLess {
		return neverMatch
	}
	type labelAlt struct {
		key, value string
		hasValue   bool
	}
	alts := make([]labelAlt, len(values))
	for i, v := range values {
		key, value, hasValue := strings.Cut(v, "=")
		alts[i] = labelAlt{key: key, value: strings.ToLower(value), hasValue: hasValue}
	}
	return func(row kube.Row) bool {
		labels, _, _ := unstructured.NestedStringMap(row.Object, "metadata", "labels")
		matched := false
		for _, alt := range alts {
			got, exists := labels[alt.key]
			if !exists {
				continue
			}
			if !alt.hasValue || strings.Contains(strings.ToLower(got), alt.value) {
				matched = true
				break
			}
		}
		if op == opNotContains {
			return !matched
		}
		return matched
	}
}

func cellAt(row kube.Row, idx int) any {
	if idx < 0 || idx >= len(row.Cells) {
		return nil
	}
	return row.Cells[idx]
}

// cellNumber reads a cell's numeric value for `>`/`<`: real numeric cells
// (incl. the joined float usage cells) directly, string cells by their
// leading numeric token so decorated values work ("3 (4m ago)" -> 3,
// "1,047" -> 1047 with the thousands separator stripped).
func cellNumber(cell any) (float64, bool) {
	if f, ok := numericCell(cell); ok {
		return f, true
	}
	return leadingNumber(cellDisplayString(cell))
}

// leadingNumber parses the leading numeric token of a string, allowing comma
// thousands separators ("1,047 (4m ago)" -> 1047).
func leadingNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	start := i
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == ',') {
		i++
	}
	if i == start {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s[:i], ",", ""), 64)
	return f, err == nil
}

// parseFilterNumber parses a `>`/`<` RHS as a plain number (whole string,
// commas allowed as thousands separators).
func parseFilterNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	return f, err == nil
}

// parseCellAgeToken parses a CELL's age for duration comparison: the leading
// space-delimited token, so decorated two-layer ages ("3m (first 41h ago)")
// compare by their primary value while RFC3339 timestamps and placeholders
// ("—", "<unknown>") fail the parse and are excluded.
func parseCellAgeToken(text string) (float64, bool) {
	token := strings.TrimSpace(text)
	if i := strings.IndexByte(token, ' '); i >= 0 {
		token = token[:i]
	}
	return parseAgeToken(token)
}

// parseAgeToken parses a kubectl-age token into seconds. apimachinery's
// HumanDuration emits one- AND two-unit tokens (59s, 5m33s, 3h, 2d3h, 1y127d),
// so the parser accepts any run of number+unit groups and sums them. Units
// are the SPEC's s/m/h/d/w/y set, lowercase only -- case-sensitivity keeps
// quantity suffixes like "100Mi" from half-parsing as durations. A bare
// number has no unit and fails, so `restarts>0` stays a numeric compare.
func parseAgeToken(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var total float64
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		if i == start || i >= len(s) {
			return 0, false
		}
		n, err := strconv.ParseFloat(s[start:i], 64)
		if err != nil {
			return 0, false
		}
		var unit float64
		switch s[i] {
		case 's':
			unit = 1
		case 'm':
			unit = 60
		case 'h':
			unit = 60 * 60
		case 'd':
			unit = 24 * 60 * 60
		case 'w':
			unit = 7 * 24 * 60 * 60
		case 'y':
			unit = 365 * 24 * 60 * 60
		default:
			return 0, false
		}
		i++
		total += n * unit
	}
	return total, true
}

// addFilterChipHref returns u with one `f=<chip>` pair APPENDED to RawQuery.
// The chip text is percent-encoded whole (url.QueryEscape), so a comma inside
// a label value arrives as %2C -- a literal comma inside one alternative, never
// an OR split -- and the append is raw string concatenation, so every sibling
// param (other raw `?f=` chips included) keeps its exact wire encoding. Used by
// the label-chip click-to-filter hrefs (SPEC §8.1).
func addFilterChipHref(u *url.URL, chip string) string {
	clone := *u
	pair := "f=" + url.QueryEscape(chip)
	if clone.RawQuery == "" {
		clone.RawQuery = pair
	} else {
		clone.RawQuery += "&" + pair
	}
	return clone.String()
}

// delQueryRawValue returns u with ONE raw occurrence of key=rawValue removed,
// operating on RawQuery directly so every other param keeps its exact raw
// encoding (a sibling chip's raw OR-commas must not be re-encoded by the
// removal href). Used by the empty-filtered state's per-chip ✕.
func delQueryRawValue(u *url.URL, key, rawValue string) string {
	clone := *u
	target := key + "=" + rawValue
	pairs := strings.Split(clone.RawQuery, "&")
	kept := make([]string, 0, len(pairs))
	removed := false
	for _, pair := range pairs {
		if !removed && pair == target {
			removed = true
			continue
		}
		if pair != "" {
			kept = append(kept, pair)
		}
	}
	clone.RawQuery = strings.Join(kept, "&")
	return clone.String()
}
