// filters-parse.test.ts -- node:test for the PURE filter-expression grammar +
// suggestion matching + the col-param merge. The operator split, the field
// resolution/aliasing, the AC ranking, and the byte-exact `?f=` survival through
// a labelcols apply are the wire-significant decisions the e2e filter-chips /
// column-visibility specs exercise through the DOM; pinning them here catches a
// grammar/encoding regression at the unit boundary.
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'`.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
    normalizeFieldName,
    fieldSuggestionText,
    splitFilterDraft,
    filterSuggestionFields,
    filterFieldKnown,
    fieldColumnIndex,
    rankFieldSuggestions,
    rankValueSuggestions,
    liveNameMatchKeys,
    mergeColParams,
    type ModelField,
    type ModelRow,
} from './filters-parse.ts';

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
    assert.equal(normalizeFieldName('Nominated-Node'), 'nominated node');
    assert.equal(normalizeFieldName('  NOMINATED NODE  '), 'nominated node');
    assert.equal(normalizeFieldName(''), '');
});

test('fieldSuggestionText dashes the lowercase label', () => {
    assert.equal(fieldSuggestionText('Nominated Node'), 'nominated-node');
    assert.equal(fieldSuggestionText('Status'), 'status');
});

// --- splitFilterDraft -------------------------------------------------------

test('free text (no operator) is null', () => {
    assert.equal(splitFilterDraft('web'), null);
    assert.equal(splitFilterDraft(''), null);
});

test('the FIRST operator splits field from value', () => {
    assert.deepEqual(splitFilterDraft('status:Running'), {
        field: 'status', op: ':', value: 'Running',
    });
    assert.deepEqual(splitFilterDraft('cpu>100m'), { field: 'cpu', op: '>', value: '100m' });
    assert.deepEqual(splitFilterDraft('node<x'), { field: 'node', op: '<', value: 'x' });
});

test('!= is recognized as a two-char operator before a later single-char op', () => {
    assert.deepEqual(splitFilterDraft('status!=Pending'), {
        field: 'status', op: '!=', value: 'Pending',
    });
    // a `<` inside the value does not re-split: != wins as the first operator.
    assert.deepEqual(splitFilterDraft('a!=b<c'), { field: 'a', op: '!=', value: 'b<c' });
});

// --- filterSuggestionFields -------------------------------------------------

test('suggestion fields exclude synthetic columns and add the virtual label/cpu/memory', () => {
    const texts = filterSuggestionFields(PODS_FIELDS).map((f) => f.text);
    assert.ok(texts.includes('status'));
    assert.ok(texts.includes('nominated-node'));
    assert.ok(!texts.includes('created'), 'hintless Created column is not suggested');
    assert.ok(texts.includes('label'), 'label is always offered');
    assert.ok(texts.includes('cpu'), 'cpu alias offered when CPU Usage exists');
    assert.ok(texts.includes('memory'), 'memory alias offered when Memory Usage exists');
});

test('bare cpu/memory CAPACITY columns are not suggested under those names', () => {
    const capacity: ModelField[] = [
        { label: 'Name', name: 'name', hint: 'string' },
        { label: 'CPU', name: 'cpu', hint: 'quantity' },     // capacity, not usage
        { label: 'Memory', name: 'memory', hint: 'quantity' },
    ];
    const texts = filterSuggestionFields(capacity).map((f) => f.text);
    // 'cpu'/'memory' come ONLY from the usage-alias branch (absent here).
    assert.ok(!texts.includes('cpu'));
    assert.ok(!texts.includes('memory'));
    assert.ok(texts.includes('label'));
});

// --- filterFieldKnown -------------------------------------------------------

test('label always resolves; typed columns resolve normalized; unknowns do not', () => {
    assert.equal(filterFieldKnown(PODS_FIELDS, 'label'), true);
    assert.equal(filterFieldKnown(PODS_FIELDS, 'Status'), true);
    assert.equal(filterFieldKnown(PODS_FIELDS, 'nominated node'), true);
    assert.equal(filterFieldKnown(PODS_FIELDS, 'bogus'), false);
    assert.equal(filterFieldKnown(PODS_FIELDS, ''), false);
});

test('cpu/memory resolve via the joined usage columns, never the capacity columns', () => {
    assert.equal(filterFieldKnown(PODS_FIELDS, 'cpu'), true); // CPU Usage present
    const capacity: ModelField[] = [
        { label: 'CPU', name: 'cpu', hint: 'quantity' },
    ];
    assert.equal(filterFieldKnown(capacity, 'cpu'), false); // capacity-only -> unknown
});

// --- fieldColumnIndex -------------------------------------------------------

test('fieldColumnIndex resolves typed columns and the usage aliases', () => {
    assert.equal(fieldColumnIndex(PODS_FIELDS, 'status'), 1);
    assert.equal(fieldColumnIndex(PODS_FIELDS, 'cpu'), 4); // -> CPU Usage column
    assert.equal(fieldColumnIndex(PODS_FIELDS, 'memory'), 5);
    assert.equal(fieldColumnIndex(PODS_FIELDS, 'bogus'), -1);
});

// --- rankFieldSuggestions ---------------------------------------------------

test('field suggestions substring-match with prefix matches ranked first', () => {
    const items = rankFieldSuggestions(PODS_FIELDS, 'no');
    // both 'nominated-node' (prefix) and 'node' substring of nothing here; the
    // prefix match must lead. 'nominated-node' starts with 'no'.
    assert.ok(items.length > 0);
    assert.equal(items[0].label, 'nominated-node');
    assert.equal(items[0].insert, 'nominated-node:');
    assert.equal(items[0].kind, 'field');
});

test('a substring match that is not a prefix ranks after a prefix match', () => {
    // 'tat' is a substring of 'status' but not a prefix; 'status' still appears.
    const items = rankFieldSuggestions(PODS_FIELDS, 'tat');
    assert.ok(items.some((i) => i.label === 'status'));
});

// --- rankValueSuggestions ---------------------------------------------------

test('value suggestions are top-N distinct by frequency descending', () => {
    const split = { field: 'status', op: ':', value: '' };
    const items = rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split);
    assert.equal(items[0].label, 'Running'); // 2 occurrences
    assert.equal(items[0].hint, '×2');
    assert.equal(items[0].insert, 'status:Running');
    assert.equal(items[0].kind, 'value');
    assert.equal(items[1].label, 'Pending');
    assert.equal(items[1].hint, '×1');
});

test('value suggestions substring-filter by the typed value', () => {
    const split = { field: 'status', op: ':', value: 'pend' };
    const items = rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split);
    assert.equal(items.length, 1);
    assert.equal(items[0].label, 'Pending');
});

test('value suggestions are empty for an unresolved column', () => {
    const split = { field: 'bogus', op: ':', value: '' };
    assert.deepEqual(rankValueSuggestions(PODS_FIELDS, PODS_ROWS, split), []);
});

// --- liveNameMatchKeys ------------------------------------------------------

test('a free-text draft narrows to the matching row keys', () => {
    const keys = liveNameMatchKeys(PODS_ROWS, 'web');
    assert.ok(keys);
    assert.deepEqual([...(keys as Set<string>)].sort(), ['c/ns/web-1', 'c/ns/web-2']);
});

test('an empty draft or a chip-in-progress is no live filter (null)', () => {
    assert.equal(liveNameMatchKeys(PODS_ROWS, ''), null);
    assert.equal(liveNameMatchKeys(PODS_ROWS, 'status:Running'), null);
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
    assert.equal(
        href,
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
    assert.equal(href, '/p?f=name:web');
});

test('an empty result query yields a bare pathname (no trailing ?)', () => {
    const owned = new Set(['labelcols', 'selector']);
    assert.equal(mergeColParams('/p', '?selector=x', owned, []), '/p');
    assert.equal(mergeColParams('/p', '', owned, []), '/p');
});
