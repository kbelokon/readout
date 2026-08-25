// filters-parse.test.ts -- Vitest for the PURE filter-expression grammar +
// suggestion matching + the col-param merge. The operator split, the field
// resolution/aliasing, the AC ranking, and the byte-exact `?f=` survival through
// a labelcols apply are the wire-significant decisions the e2e filter-chips /
// column-visibility specs exercise through the DOM; pinning them here catches a
// grammar/encoding regression at the unit boundary.
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import {
    fieldColumnIndex,
    fieldSuggestionText,
    filterFieldKnown,
    filterSuggestionFields,
    liveNameMatchKeys,
    type ModelField,
    type ModelRow,
    mergeColParams,
    normalizeFieldName,
    rankFieldSuggestions,
    rankValueSuggestions,
    splitFilterDraft,
} from './filters-parse.js';

// A pod-like model: Name + the data-hint Status/Node columns, plus a synthetic
// (hintless) Created column and the joined CPU/Memory usage columns.
const PODS_FIELDS: ModelField[] = [
    { label: 'Name', name: 'name', hint: 'string' },
    { label: 'Status', name: 'status', hint: 'enum' },
    { label: 'Nominated Node', name: 'nominated-node', hint: 'string' },
    { label: 'Created', name: 'created', hint: '' }, // synthetic, not filterable
    { label: 'CPU Usage', name: 'cpu-usage', hint: 'quantity' },
    { label: 'Memory Usage', name: 'memory-usage', hint: 'quantity' },
];

const PODS_ROWS: ModelRow[] = [
    { key: 'c/ns/web-1', name: 'web-1', cells: ['web-1', 'Running', '', '', '10m', '20Mi'] },
    { key: 'c/ns/web-2', name: 'web-2', cells: ['web-2', 'Running', '', '', '12m', '22Mi'] },
    { key: 'c/ns/db-1', name: 'db-1', cells: ['db-1', 'Pending', '', '', '5m', '10Mi'] },
];

// --- normalizeFieldName / fieldSuggestionText -------------------------------

test('normalizeFieldName lowercases, dashes->spaces, trims', () => {
    expect(normalizeFieldName('Nominated-Node')).toBe('nominated node');
    expect(normalizeFieldName('  NOMINATED NODE  ')).toBe('nominated node');
    expect(normalizeFieldName('')).toBe('');
});

test('fieldSuggestionText dashes the lowercase label', () => {
    expect(fieldSuggestionText('Nominated Node')).toBe('nominated-node');
    expect(fieldSuggestionText('Status')).toBe('status');
});

// --- splitFilterDraft -------------------------------------------------------

test('free text (no operator) is null', () => {
    expect(splitFilterDraft('web')).toBe(null);
    expect(splitFilterDraft('')).toBe(null);
});

test('the FIRST operator splits field from value', () => {
    expect(splitFilterDraft('status:Running')).toStrictEqual({
        field: 'status',
        op: ':',
        value: 'Running',
    });
    expect(splitFilterDraft('cpu>100m')).toStrictEqual({ field: 'cpu', op: '>', value: '100m' });
    expect(splitFilterDraft('node<x')).toStrictEqual({ field: 'node', op: '<', value: 'x' });
});

test('!= is recognized as a two-char operator before a later single-char op', () => {
    expect(splitFilterDraft('status!=Pending')).toStrictEqual({
        field: 'status',
        op: '!=',
        value: 'Pending',
    });
    // a `<` inside the value does not re-split: != wins as the first operator.
    expect(splitFilterDraft('a!=b<c')).toStrictEqual({ field: 'a', op: '!=', value: 'b<c' });
});

// --- filterSuggestionFields -------------------------------------------------

test('suggestion fields exclude synthetic columns and add the virtual label/cpu/memory', () => {
    const texts = filterSuggestionFields(PODS_FIELDS).map((f) => f.text);
    expect(texts).toContain('status');
    expect(texts).toContain('nominated-node');
    expect(texts, 'hintless Created column is not suggested').not.toContain('created');
    expect(texts, 'label is always offered').toContain('label');
    expect(texts, 'cpu alias offered when CPU Usage exists').toContain('cpu');
    expect(texts, 'memory alias offered when Memory Usage exists').toContain('memory');
});

test('bare cpu/memory CAPACITY columns are not suggested under those names', () => {
    const capacity: ModelField[] = [
        { label: 'Name', name: 'name', hint: 'string' },
        { label: 'CPU', name: 'cpu', hint: 'quantity' }, // capacity, not usage
        { label: 'Memory', name: 'memory', hint: 'quantity' },
    ];
    const texts = filterSuggestionFields(capacity).map((f) => f.text);
    // 'cpu'/'memory' come ONLY from the usage-alias branch (absent here).
    expect(texts).not.toContain('cpu');
    expect(texts).not.toContain('memory');
    expect(texts).toContain('label');
});

// --- filterFieldKnown -------------------------------------------------------

test('label always resolves; typed columns resolve normalized; unknowns do not', () => {
    expect(filterFieldKnown(PODS_FIELDS, 'label')).toBe(true);
    expect(filterFieldKnown(PODS_FIELDS, 'Status')).toBe(true);
    expect(filterFieldKnown(PODS_FIELDS, 'nominated node')).toBe(true);
    expect(filterFieldKnown(PODS_FIELDS, 'bogus')).toBe(false);
    expect(filterFieldKnown(PODS_FIELDS, '')).toBe(false);
});

test('cpu/memory resolve via the joined usage columns, never the capacity columns', () => {
    expect(filterFieldKnown(PODS_FIELDS, 'cpu')).toBe(true); // CPU Usage present
    const capacity: ModelField[] = [{ label: 'CPU', name: 'cpu', hint: 'quantity' }];
    expect(filterFieldKnown(capacity, 'cpu')).toBe(false); // capacity-only -> unknown
});

// --- fieldColumnIndex -------------------------------------------------------

test('fieldColumnIndex resolves typed columns and the usage aliases', () => {
    expect(fieldColumnIndex(PODS_FIELDS, 'status')).toBe(1);
    expect(fieldColumnIndex(PODS_FIELDS, 'cpu')).toBe(4); // -> CPU Usage column
    expect(fieldColumnIndex(PODS_FIELDS, 'memory')).toBe(5);
    expect(fieldColumnIndex(PODS_FIELDS, 'bogus')).toBe(-1);
});

// --- rankFieldSuggestions ---------------------------------------------------

test('field suggestions substring-match with prefix matches ranked first', () => {
    const items = rankFieldSuggestions(PODS_FIELDS, 'no');
    // both 'nominated-node' (prefix) and 'node' substring of nothing here; the
    // prefix match must lead. 'nominated-node' starts with 'no'.
    expect(items).not.toHaveLength(0);
    expect(items[0].label).toBe('nominated-node');
    expect(items[0].insert).toBe('nominated-node:');
    expect(items[0].kind).toBe('field');
});

test('a substring match that is not a prefix ranks after a prefix match', () => {
    // 'tat' is a substring of 'status' but not a prefix; 'status' still appears.
    const items = rankFieldSuggestions(PODS_FIELDS, 'tat');
    expect(items.map((item) => item.label)).toContain('status');
});

// --- rankValueSuggestions ---------------------------------------------------

test('value suggestions are top-N distinct by frequency descending', () => {
    const split = { field: 'status', op: ':', value: '' };
    const items = rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split);
    expect(items[0].label).toBe('Running'); // 2 occurrences
    expect(items[0].hint).toBe('×2');
    expect(items[0].insert).toBe('status:Running');
    expect(items[0].kind).toBe('value');
    expect(items[1].label).toBe('Pending');
    expect(items[1].hint).toBe('×1');
});

test('value suggestions substring-filter by the typed value', () => {
    const split = { field: 'status', op: ':', value: 'pend' };
    const items = rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split);
    expect(items.length).toBe(1);
    expect(items[0].label).toBe('Pending');
});

test('value suggestions are empty for an unresolved column', () => {
    const split = { field: 'bogus', op: ':', value: '' };
    expect(rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split)).toStrictEqual([]);
});

// --- liveNameMatchKeys ------------------------------------------------------

test('a free-text draft narrows to the matching row keys', () => {
    const keys = liveNameMatchKeys(PODS_ROWS, 'web');
    expect(keys).not.toBeNull();
    expect([...(keys as Set<string>)].sort()).toStrictEqual(['c/ns/web-1', 'c/ns/web-2']);
});

test('an empty draft or a chip-in-progress is no live filter (null)', () => {
    expect(liveNameMatchKeys(PODS_ROWS, '')).toBe(null);
    expect(liveNameMatchKeys(PODS_ROWS, 'status:Running')).toBe(null);
});

// --- mergeColParams ---------------------------------------------------------

test('merge keeps un-owned pairs byte-exact (raw ?f= commas survive)', () => {
    // an active OR-chip f=status:Running,Pending plus a sort -- both un-owned.
    const owned = new Set(['labelcols', 'selector', 'filter']);
    const fields = ['labelcols=app'];
    const href = mergeColParams(
        '/clusters/c/namespaces/ns/pods',
        '?f=status:Running,Pending&sort=name',
        owned,
        fields,
    );
    // the raw comma in the f= chip is preserved verbatim (no %2C), order frozen.
    expect(href).toBe(
        '/clusters/c/namespaces/ns/pods?f=status:Running,Pending&sort=name&labelcols=app',
    );
});

test('a cleared visible input drops its pair (owned but no field contributed)', () => {
    const owned = new Set(['labelcols', 'selector', 'filter']);
    const href = mergeColParams(
        '/p',
        '?selector=app%3Dnginx&f=name:web',
        owned,
        [], // both visible inputs cleared
    );
    // selector (owned) is dropped; f= (un-owned) survives byte-exact.
    expect(href).toBe('/p?f=name:web');
});

test('an empty result query yields a bare pathname (no trailing ?)', () => {
    const owned = new Set(['labelcols', 'selector']);
    expect(mergeColParams('/p', '?selector=x', owned, [])).toBe('/p');
    expect(mergeColParams('/p', '', owned, [])).toBe('/p');
});
