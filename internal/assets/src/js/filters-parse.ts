// filters-parse.ts -- the PURE expression parsing + suggestion matching of the
// v2 filter chips editor (D7), extracted from legacy.js so the operator split,
// the field-name normalization, the autocomplete ranking, and the value-
// frequency scan are node-tested in isolation. No DOM: every function takes a
// plain string or a plain row-model object, so a regression in the grammar (the
// part the e2e filter-chips spec only sees through rendered chips) is caught at
// the unit boundary.
//
// The model shapes mirror what filters.ts harvests from the rendered table. The
// matching/aliasing rules mirror the SERVER (filter.go's resolveFilterColumn +
// splitFilterOperator): typed fields resolve case-insensitively with dashes and
// spaces interchangeable; `label` always resolves; `cpu`/`memory` bind ONLY the
// joined usage columns, never the capacity columns; the FIRST operator
// occurrence splits field from value. Keeping these here lets node:test pin them
// against the documented server behavior without a browser.

// A captured table column. `hint` is '' for synthetic/non-filterable columns
// (Created, Cluster, Namespace) -- they align cells but never suggest.
export interface ModelField {
    label: string;
    name: string;
    hint: string;
}

// A captured identity row. `cells` align positionally with the field list.
export interface ModelRow {
    key: string;
    name: string;
    cells: string[];
}

// A draft split into a field/op/value (a chip-in-progress), or null for free
// text (no operator) which never commits -- it live-matches names only.
export interface DraftSplit {
    field: string;
    op: string;
    value: string;
}

// An autocomplete entry. `kind` drives accept behavior: a 'field' readies the
// value (`field:` + value suggestions), a complete 'value' commits on ⏎.
export interface ACItem {
    label: string;
    hint: string;
    insert: string;
    kind: 'field' | 'value';
}

// normalizeFieldName mirrors the server's resolveFilterColumn: lowercase, dashes
// to spaces, trimmed -- so "Nominated-Node" / "nominated node" / "NOMINATED NODE"
// all resolve to the one canonical key.
export function normalizeFieldName(s: string): string {
    return (s || '').toLowerCase().replace(/-/g, ' ').trim();
}

// fieldSuggestionText turns a column label into the dashed lowercase form the
// server resolves ("Nominated Node" -> "nominated-node").
export function fieldSuggestionText(label: string): string {
    return (label || '').toLowerCase().trim().replace(/\s+/g, '-');
}

// splitFilterDraft mirrors splitFilterOperator: the FIRST operator occurrence
// (`!=` / `:` / `>` / `<`) splits field from value. null = free text. `!=` is
// checked before the single-char operators so "a!=b" splits on `!=`, not a
// stray `<`/`>`/`:` later in the value.
export function splitFilterDraft(s: string): DraftSplit | null {
    for (let i = 0; i < s.length; i++) {
        const c = s[i];
        if (c === '!' && s[i + 1] === '=') {
            return { field: s.slice(0, i).trim(), op: '!=', value: s.slice(i + 2) };
        }
        if (c === ':' || c === '>' || c === '<') {
            return { field: s.slice(0, i).trim(), op: c, value: s.slice(i + 1) };
        }
    }
    return null;
}

// hasModelColumn: a filterable (hinted) column whose normalized label matches.
export function hasModelColumn(fields: ModelField[], normName: string): boolean {
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === normName);
}

// filterSuggestionFields: the fields autocomplete offers. Every data-hint column
// EXCEPT the bare cpu/memory capacity columns (the server's cpu/memory aliases
// bind only the joined usage columns -- suggesting the capacity column under
// those names would commit chips matching zero rows), plus the virtual fields:
// `label` always, the cpu/memory aliases when the metrics-join usage columns
// exist.
export function filterSuggestionFields(fields: ModelField[]): { text: string; hint: string }[] {
    const out: { text: string; hint: string }[] = [];
    fields.forEach((f) => {
        if (!f.hint) {
            return;
        }
        const norm = normalizeFieldName(f.label);
        if (norm === 'cpu' || norm === 'memory') {
            return; // capacity columns: the alias never binds them (filter.go)
        }
        out.push({ text: f.name, hint: f.hint });
    });
    out.push({ text: 'label', hint: 'key=value' });
    if (hasModelColumn(fields, 'cpu usage')) {
        out.push({ text: 'cpu', hint: 'quantity' });
    }
    if (hasModelColumn(fields, 'memory usage')) {
        out.push({ text: 'memory', hint: 'quantity' });
    }
    return out;
}

// filterFieldKnown mirrors the server's field resolution: `label` always
// resolves; `cpu`/`memory` resolve ONLY via the joined usage columns; everything
// else against the data-hint headers.
export function filterFieldKnown(fields: ModelField[], field: string): boolean {
    const want = normalizeFieldName(field);
    if (!want) {
        return false;
    }
    if (want === 'label') {
        return true;
    }
    if (want === 'cpu' || want === 'memory') {
        return hasModelColumn(fields, want + ' usage');
    }
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === want);
}

// fieldColumnIndex resolves a typed field to its model column index (for the
// value autocomplete), applying the same cpu/memory usage aliasing. -1 = unknown.
export function fieldColumnIndex(fields: ModelField[], field: string): number {
    let want = normalizeFieldName(field);
    if (want === 'cpu' || want === 'memory') {
        want += ' usage';
    }
    for (let i = 0; i < fields.length; i++) {
        const f = fields[i];
        if (f.hint && normalizeFieldName(f.label) === want) {
            return i;
        }
    }
    return -1;
}

// rankFieldSuggestions builds the field-name AC items for a no-operator draft:
// substring match on the normalized name, prefix matches ranked first (a stable
// sort keeps the schema order within each tier).
export function rankFieldSuggestions(fields: ModelField[], draft: string): ACItem[] {
    const q = normalizeFieldName(draft);
    const matched = filterSuggestionFields(fields).filter(
        (f) => normalizeFieldName(f.text).indexOf(q) !== -1,
    );
    matched.sort((a, b) => {
        const ap = normalizeFieldName(a.text).indexOf(q) === 0 ? 0 : 1;
        const bp = normalizeFieldName(b.text).indexOf(q) === 0 ? 0 : 1;
        return ap - bp;
    });
    return matched.map((f) => ({
        label: f.text,
        hint: f.hint,
        insert: f.text + ':',
        kind: 'field' as const,
    }));
}

// rankValueSuggestions builds the value AC items for a `field:` equality draft on
// a known real column: the top 8 distinct values by frequency over the FULL row
// model, optionally substring-filtered by the typed value. Frequency descending;
// the slice-8 cap matches the editor. Returns [] when the column is unresolved.
export function rankValueSuggestions(
    fields: ModelField[],
    rows: ModelRow[],
    split: DraftSplit,
): ACItem[] {
    const idx = fieldColumnIndex(fields, split.field);
    if (idx < 0) {
        return [];
    }
    const freq = new Map<string, number>();
    rows.forEach((row) => {
        const v = row.cells[idx];
        if (v) {
            freq.set(v, (freq.get(v) || 0) + 1);
        }
    });
    const typed = split.value.trim().toLowerCase();
    let entries = Array.from(freq.entries());
    if (typed) {
        entries = entries.filter(([v]) => v.toLowerCase().indexOf(typed) !== -1);
    }
    entries.sort((a, b) => b[1] - a[1]);
    return entries.slice(0, 8).map(([v, n]) => ({
        label: v,
        hint: '×' + n,
        insert: split.field.trim() + ':' + v,
        kind: 'value' as const,
    }));
}

// liveNameMatchKeys computes the visible-key SET for a free-text draft: the keys
// of rows whose name contains the draft (case-insensitive). null = no live
// filter (empty draft, or a draft with an operator -- a chip in progress narrows
// nothing). The MATCH runs on the full row model; the DOM application is the
// caller's job.
export function liveNameMatchKeys(rows: ModelRow[], draft: string): Set<string> | null {
    const text = (!draft || splitFilterDraft(draft)) ? '' : draft.trim().toLowerCase();
    if (!text) {
        return null;
    }
    const visible = new Set<string>();
    rows.forEach((row) => {
        if (row.name.toLowerCase().indexOf(text) !== -1) {
            visible.add(row.key);
        }
    });
    return visible;
}

// mergeColParams MERGES a popover form's owned fields into the live query
// instead of replacing it wholesale (the D8 labelcols/selector apply). Every
// existing query pair whose key the form does NOT own survives BYTE-EXACT --
// above all the `?f=` chips, whose raw OR-commas are wire-significant (filter.go
// splits alternatives on raw commas BEFORE percent-decoding, so a re-encoded
// %2C would turn an OR into a literal comma). This is a deliberate STRING
// concatenation over the raw query, NOT URLSearchParams: the parameter order +
// raw encoding are frozen until the end of the refactor.
//
// `search` is location.search (with or without the leading '?'); `owned` is the
// set of param names the form replaces; `fields` are the already-encoded
// `name=value` pairs for the form's non-empty visible inputs. A cleared visible
// input drops its pair (its name is owned but contributes no field), exactly
// like the native blank-empty-names path. Returns pathname + ('?' + query) or
// just pathname when nothing remains.
export function mergeColParams(
    pathname: string,
    search: string,
    owned: Set<string>,
    fields: string[],
): string {
    const kept: string[] = [];
    search.replace(/^\?/, '').split('&').forEach((pair) => {
        if (pair && !owned.has(pair.split('=')[0])) {
            kept.push(pair); // byte-exact survival (raw f= commas included)
        }
    });
    const query = kept.concat(fields).join('&');
    return pathname + (query ? '?' + query : '');
}
