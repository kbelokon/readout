// collapse-hash.test.ts -- Vitest for the PURE collapse-hash parser
// (parseCollapsedNames). The section-collapse feature round-trips through the
// URL fragment (#collapsed=a,b,c); the parser is the read half, so a regression
// here silently drops the on-load restore. Pinned directly (no DOM).
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import { parseCollapsedNames } from './collapse-hash.js';

test('no hash / empty hash yields no names', () => {
    expect(parseCollapsedNames('')).toStrictEqual([]);
    expect(parseCollapsedNames('#')).toStrictEqual([]);
});

test('a single collapsed param yields its comma list', () => {
    expect(parseCollapsedNames('#collapsed=spec')).toStrictEqual(['spec']);
    expect(parseCollapsedNames('#collapsed=spec,status,metadata')).toStrictEqual([
        'spec',
        'status',
        'metadata',
    ]);
});

test('the leading # is optional', () => {
    expect(parseCollapsedNames('collapsed=spec,status')).toStrictEqual(['spec', 'status']);
});

test('only a leading # is structural; a # inside a name is preserved', () => {
    expect(parseCollapsedNames('collapsed=spec#details,status')).toStrictEqual([
        'spec#details',
        'status',
    ]);
});

test('collapsed is selected out of a multi-param fragment', () => {
    // The fragment is a `;`-separated list of key=value params; only the
    // `collapsed` value contributes names.
    expect(parseCollapsedNames('#line=12;collapsed=spec,status;other=x')).toStrictEqual([
        'spec',
        'status',
    ]);
});

test('a missing or empty collapsed value yields no names', () => {
    expect(parseCollapsedNames('#line=12')).toStrictEqual([]);
    expect(parseCollapsedNames('#collapsed=')).toStrictEqual([]);
    expect(parseCollapsedNames('#collapsed')).toStrictEqual([]);
});

test('empty entries between commas are dropped', () => {
    expect(parseCollapsedNames('#collapsed=spec,,status,')).toStrictEqual(['spec', 'status']);
});

test('the write-path round-trips: collapsed=<join(",")> parses back to the names', () => {
    // The .collapsible h4.title write builds `collapsed=${names.join(',')}`;
    // parseCollapsedNames must reverse exactly that shape.
    const names = ['spec', 'status', 'events'];
    expect(parseCollapsedNames(`#collapsed=${names.join(',')}`)).toStrictEqual(names);
});
