// collapse-hash.test.ts -- node:test for the PURE collapse-hash parser
// (parseCollapsedNames). The section-collapse feature round-trips through the
// URL fragment (#collapsed=a,b,c); the parser is the read half, so a regression
// here silently drops the on-load restore. Pinned directly (no DOM).
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'`.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { parseCollapsedNames } from './collapse-hash.ts';

test('no hash / empty hash yields no names', () => {
    assert.deepEqual(parseCollapsedNames(''), []);
    assert.deepEqual(parseCollapsedNames('#'), []);
});

test('a single collapsed param yields its comma list', () => {
    assert.deepEqual(parseCollapsedNames('#collapsed=spec'), ['spec']);
    assert.deepEqual(parseCollapsedNames('#collapsed=spec,status,metadata'), [
        'spec',
        'status',
        'metadata',
    ]);
});

test('the leading # is optional', () => {
    assert.deepEqual(parseCollapsedNames('collapsed=spec,status'), ['spec', 'status']);
});

test('collapsed is selected out of a multi-param fragment', () => {
    // The fragment is a `;`-separated list of key=value params; only the
    // `collapsed` value contributes names.
    assert.deepEqual(parseCollapsedNames('#line=12;collapsed=spec,status;other=x'), [
        'spec',
        'status',
    ]);
});

test('a missing or empty collapsed value yields no names', () => {
    assert.deepEqual(parseCollapsedNames('#line=12'), []);
    assert.deepEqual(parseCollapsedNames('#collapsed='), []);
    assert.deepEqual(parseCollapsedNames('#collapsed'), []);
});

test('empty entries between commas are dropped', () => {
    assert.deepEqual(parseCollapsedNames('#collapsed=spec,,status,'), ['spec', 'status']);
});

test('the write-path round-trips: collapsed=<join(",")> parses back to the names', () => {
    // The .collapsible h4.title write builds `collapsed=${names.join(',')}`;
    // parseCollapsedNames must reverse exactly that shape.
    const names = ['spec', 'status', 'events'];
    assert.deepEqual(parseCollapsedNames(`#collapsed=${names.join(',')}`), names);
});
