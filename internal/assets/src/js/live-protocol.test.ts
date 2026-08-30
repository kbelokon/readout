// @vitest-environment jsdom

import { describe, expect, test } from 'vitest';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    LIST_DELTA_APPLIED_EVENT,
    type LiveV2DeltaEnvelope,
    type LiveV2Envelope,
    type LiveV2SnapshotEnvelope,
    type LiveV2TerminalEnvelope,
} from './live-protocol.js';

function snapshot(overrides: Partial<LiveV2SnapshotEnvelope> = {}): LiveV2SnapshotEnvelope {
    return {
        v: 2,
        kind: 'snapshot',
        g: 'generation-1',
        seq: 1,
        rev: 'rev-1',
        rv: '10',
        schema: 'schema-1',
        snapshot: { html: '<div id="resource-list-content"></div>' },
        ...overrides,
    };
}

function delta(overrides: Partial<LiveV2DeltaEnvelope> = {}): LiveV2DeltaEnvelope {
    return {
        v: 2,
        kind: 'delta',
        g: 'generation-1',
        seq: 2,
        rev: 'rev-2',
        rv: '11',
        schema: 'schema-1',
        delta: {
            base: 'rev-1',
            rev: 'rev-2',
            upsert: [
                {
                    key: 'dev/a',
                    row: '<tr data-key="dev/a"><td><span style="--kh:42">Unknown</span></td></tr>',
                },
            ],
        },
        ...overrides,
    };
}

function terminal(overrides: Partial<LiveV2TerminalEnvelope> = {}): LiveV2TerminalEnvelope {
    return {
        v: 2,
        kind: 'terminal',
        g: 'generation-1',
        seq: 2,
        rev: 'rev-1',
        schema: 'schema-1',
        reason: 'shutdown',
        ...overrides,
    };
}

function decode(value: unknown) {
    return decodeLiveV2Envelope(JSON.stringify(value));
}

function expectInvalid(value: unknown): void {
    expect(decode(value)).toStrictEqual({ ok: false });
}

function invalidDelta(body: Record<string, unknown>): LiveV2DeltaEnvelope {
    return delta({
        delta: { base: 'rev-1', rev: 'rev-2', ...body } as LiveV2DeltaEnvelope['delta'],
    });
}

describe('Live v2 envelope schema', () => {
    test('pins the public post-delta event name', () => {
        expect(LIST_DELTA_APPLIED_EVENT).toBe('ro:list-delta-applied');
    });

    test.each<[string, LiveV2Envelope]>([
        ['snapshot', snapshot()],
        ['delta', delta()],
        ['terminal', terminal()],
    ])('decodes a valid %s envelope', (_name, input) => {
        const result = decode(input);

        expect(result).toMatchObject({ ok: true, value: input });
    });

    test('uses the v2 fields only and rejects unknown envelope members', () => {
        expectInvalid({ ...snapshot(), screen: '/legacy-screen' });
        expectInvalid({ ...snapshot(), future: true });
        expectInvalid({ ...delta(), future: true });
        expectInvalid({ ...snapshot(), snapshot: { html: '<div></div>', future: true } });
    });

    test.each([
        null,
        [],
        {},
        { ...snapshot(), v: 3 },
        { ...snapshot(), g: '' },
        { ...snapshot(), seq: 0 },
        { ...snapshot(), seq: 1.5 },
        { ...snapshot(), kind: 'future' },
        { ...snapshot(), snapshot: null },
        { ...snapshot(), snapshot: { html: '' } },
        { ...delta(), delta: null },
        { ...terminal(), rev: '' },
        { ...terminal(), rv: '' },
        { ...terminal(), schema: '' },
        { ...terminal(), kind: 'future' },
        { ...terminal(), reason: 'future' },
        // `idle` was retired with the per-subscriber idle cap: the hub owns the
        // watch now, so a quiet stream is never closed for being quiet.
        { ...terminal(), reason: 'idle' },
    ])('rejects malformed schema %#', (input) => {
        expectInvalid(input);
    });

    test('relies on JSON.parse for duplicate object members instead of a second JSON lexer', () => {
        const frame =
            '{"v":1,"v":2,"kind":"terminal","g":"generation-1","seq":2,"reason":"shutdown"}';

        expect(decodeLiveV2Envelope(frame)).toMatchObject({
            ok: true,
            value: { v: 2, kind: 'terminal', reason: 'shutdown' },
        });
    });

    test('accepts server-owned HTML without fragment-specific size or markup schemas', () => {
        const html = `<tr data-key="dev/a"><td>${'<span style="--kh:7">Kind</span>'.repeat(5000)}</td></tr>`;
        const input = delta({ delta: { ...delta().delta, upsert: [{ key: 'dev/a', row: html }] } });

        expect(decode(input)).toMatchObject({
            ok: true,
            value: { delta: { upsert: [{ key: 'dev/a', row: html }] } },
        });
    });

    test.each(['auth', 'lifetime', 'shutdown', 'watch-failed'] as const)(
        'accepts the terminal reason %s',
        (reason) => {
            expect(decode(terminal({ reason }))).toMatchObject({
                ok: true,
                value: { kind: 'terminal', reason },
            });
        },
    );
});

describe('delta collection schema', () => {
    test('decodes the closed remove/upsert/order/region shapes', () => {
        const input = delta({
            delta: {
                base: 'rev-1',
                rev: 'rev-2',
                remove: [{ key: 'dev/a', cause: 'project' }],
                upsert: [
                    {
                        key: 'dev/b',
                        row: '<tr data-key="dev/b"><td>B</td></tr>',
                        card: '<article data-key="dev/b">B</article>',
                    },
                ],
                order: ['dev/b'],
                regions: [
                    {
                        region: 'count',
                        html: '<strong data-ro-live-region="count">1</strong>',
                    },
                ],
            },
        });

        expect(decode(input)).toMatchObject({ ok: true, value: input });
    });

    test.each([
        {
            remove: [
                { key: 'dev/a', cause: 'delete' },
                { key: 'dev/a', cause: 'project' },
            ],
        },
        {
            upsert: [
                { key: 'dev/a', row: '<tr data-key="dev/a"></tr>' },
                { key: 'dev/a', row: '<tr data-key="dev/a"></tr>' },
            ],
        },
        { order: ['dev/a', 'dev/a'] },
        {
            regions: [
                { region: 'count', html: '<span>1</span>' },
                { region: 'count', html: '<span>2</span>' },
            ],
        },
        {
            remove: [{ key: 'dev/a', cause: 'delete' }],
            upsert: [
                { key: 'dev/a', row: '<tr data-key="dev/a"></tr>' },
                { key: 'dev/b', row: '<tr data-key="dev/b"></tr>' },
            ],
        },
    ])('rejects duplicate or overlapping semantic keys %#', (body) => {
        expectInvalid(invalidDelta(body));
    });

    test.each([
        { remove: [{ key: 'dev/a', cause: 'delete', future: true }] },
        { remove: [{ key: '', cause: 'delete' }] },
        { remove: [{ key: 'dev/a', cause: 'unknown' }] },
        { upsert: [{ key: 'dev/a', row: '<tr></tr>', future: true }] },
        { upsert: [{ key: 'dev/a', row: '' }] },
        { upsert: [{ key: 'dev/a', row: '<tr></tr>', card: '' }] },
        { order: [''] },
        { regions: [{ region: 'count', html: '<span></span>', future: true }] },
        { regions: [{ region: 'unknown', html: '<span></span>' }] },
        { regions: [{ region: 'count', html: '' }] },
    ])('rejects invalid collection fields %#', (body) => {
        expectInvalid(invalidDelta(body));
    });

    // A malformed CONTAINER, not a malformed item. Without the Array.isArray
    // guard `order: 'dev/a'` would iterate the string's characters into a
    // plausible-looking three-key order and silently reorder the table.
    test.each([
        { remove: {} },
        { remove: 'dev/a' },
        { upsert: null },
        { upsert: { key: 'dev/a', row: '<tr></tr>' } },
        { order: 'dev/a' },
        { order: 7 },
        { regions: 7 },
        { regions: { region: 'count', html: '<span></span>' } },
    ])('rejects a collection field that is not an array %#', (body) => {
        expectInvalid(invalidDelta(body));
    });

    test('rejects a delta revision that differs from its envelope revision', () => {
        expectInvalid(invalidDelta({ rev: 'other' }));
    });
});

test('apply accepts only a decoded delta or a raw valid delta frame', () => {
    const cursor = { g: 'generation-1', seq: 1, rev: 'rev-1', schema: 'schema-1' };

    expect(applyLiveV2Delta(snapshot(), cursor)).toStrictEqual({ ok: false });
    expect(applyLiveV2Delta(null, cursor)).toStrictEqual({ ok: false });
});
