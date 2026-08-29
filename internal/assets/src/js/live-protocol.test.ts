// @vitest-environment jsdom

import { describe, expect, test } from 'vitest';
import { decodeLiveV2Envelope, LIVE_V2_LIMITS, type LiveV2DeltaEnvelope } from './live-protocol.js';

function delta(overrides: Record<string, unknown> = {}): Record<string, unknown> {
    return {
        v: 2,
        kind: 'delta',
        g: 'generation-1',
        seq: 2,
        screen: '/pods?ns=dev',
        rev: 'rev-2',
        rv: '42',
        schema: 'schema-a',
        delta: {
            base: 'rev-1',
            rev: 'rev-2',
            regions: [
                {
                    region: 'count',
                    html: '<span class="ro-count" data-ro-live-region="count">1</span>',
                },
            ],
        },
        ...overrides,
    };
}

function decode(value: unknown) {
    return decodeLiveV2Envelope(JSON.stringify(value));
}

describe('strict Live v2 decoder', () => {
    test('decodes the complete delta contract without inventing optional fields', () => {
        const result = decode(
            delta({
                schema: 'schema-a',
                delta: {
                    base: 'rev-1',
                    rev: 'rev-2',
                    remove: [{ key: 'dev/pods/gone', cause: 'delete' }],
                    upsert: [
                        {
                            key: 'dev/pods/api',
                            row: '<tr id="row-api" data-key="dev/pods/api"><td>api</td></tr>',
                            card: '<div class="ro-pcard" data-key="dev/pods/api">api</div>',
                        },
                    ],
                    order: ['dev/pods/api'],
                    regions: [
                        {
                            region: 'count',
                            html: '<span data-ro-live-region="count">1</span>',
                        },
                    ],
                },
            }),
        );

        expect(result.ok).toBe(true);
        const value = (result as { ok: true; value: LiveV2DeltaEnvelope }).value;
        expect(value.delta.remove).toStrictEqual([{ key: 'dev/pods/gone', cause: 'delete' }]);
        expect(value.delta.upsert?.[0]?.card).toContain('ro-pcard');
        expect(value.schema).toBe('schema-a');
        expect(Object.isFrozen(value)).toBe(true);
        expect(Object.isFrozen(value.delta)).toBe(true);
        expect(Object.isFrozen(value.delta.upsert)).toBe(true);
        expect(Object.isFrozen(value.delta.upsert?.[0])).toBe(true);
    });

    test.each([
        {
            kind: 'snapshot',
            value: {
                v: 2,
                kind: 'snapshot',
                g: 'g',
                seq: 1,
                screen: '/pods',
                rev: 'r1',
                schema: 'schema-a',
                snapshot: { html: '<div>snapshot</div>' },
            },
        },
        {
            kind: 'terminal',
            value: {
                v: 2,
                kind: 'terminal',
                g: 'g',
                seq: 3,
                screen: '/pods',
                reason: 'shutdown',
            },
        },
    ])('decodes a strict $kind envelope', ({ kind, value }) => {
        const result = decode(value);
        expect(result.ok).toBe(true);
        if (result.ok) expect(result.value.kind).toBe(kind);
    });

    test.each([
        ['not JSON', '{'],
        ['non-object', '[]'],
        ['wrong version', JSON.stringify(delta({ v: 3 }))],
        ['non-integral sequence', JSON.stringify(delta({ seq: 2.5 }))],
        ['unsafe sequence', JSON.stringify(delta({ seq: Number.MAX_SAFE_INTEGER + 1 }))],
        ['bad generation', JSON.stringify(delta({ g: 'spaces fail' }))],
        ['unknown kind', JSON.stringify(delta({ kind: 'patch' }))],
        ['unknown envelope field', JSON.stringify(delta({ surprise: true }))],
        [
            'unknown nested field',
            JSON.stringify(delta({ delta: { base: 'rev-1', rev: 'rev-2', nope: [] } })),
        ],
        [
            'revision disagreement',
            JSON.stringify(delta({ delta: { base: 'rev-1', rev: 'other' } })),
        ],
        [
            'unknown remove cause',
            JSON.stringify(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        remove: [{ key: 'a', cause: 'gone' }],
                    },
                }),
            ),
        ],
        [
            'unknown terminal reason',
            JSON.stringify({
                v: 2,
                kind: 'terminal',
                g: 'g',
                seq: 1,
                screen: '/pods',
                reason: 'retry',
            }),
        ],
    ])('rejects %s without throwing', (_name, frame) => {
        expect(decodeLiveV2Envelope(frame).ok).toBe(false);
    });

    test('rejects duplicate JSON member names at any object depth', () => {
        const raw =
            '{"v":2,"kind":"delta","g":"g","seq":2,"screen":"/pods",' +
            '"rev":"r2","schema":"s","delta":{"base":"r1","rev":"r2","rev":"r3"}}';
        expect(decodeLiveV2Envelope(raw)).toMatchObject({
            ok: false,
            error: { code: 'duplicate' },
        });
    });

    test.each([
        {
            name: 'remove key',
            body: {
                remove: [
                    { key: 'a', cause: 'delete' },
                    { key: 'a', cause: 'project' },
                ],
            },
        },
        {
            name: 'upsert key',
            body: {
                upsert: [
                    { key: 'a', row: '<tr></tr>' },
                    { key: 'a', row: '<tr></tr>' },
                ],
            },
        },
        { name: 'order key', body: { order: ['a', 'a'] } },
        {
            name: 'region',
            body: {
                regions: [
                    { region: 'count', html: '<span></span>' },
                    { region: 'count', html: '<span></span>' },
                ],
            },
        },
        {
            name: 'remove/upsert overlap',
            body: {
                remove: [{ key: 'a', cause: 'delete' }],
                upsert: [{ key: 'a', row: '<tr></tr>' }],
            },
        },
    ])('rejects duplicate $name operations', ({ body }) => {
        const result = decode(delta({ delta: { base: 'rev-1', rev: 'rev-2', ...body } }));
        expect(result).toMatchObject({ ok: false, error: { code: 'duplicate' } });
    });

    test('enforces string, frame and operation bounds', () => {
        expect(decode(delta({ g: 'g'.repeat(LIVE_V2_LIMITS.generationLength + 1) }))).toMatchObject(
            {
                ok: false,
                error: { code: 'limit-exceeded' },
            },
        );
        expect(decodeLiveV2Envelope(' '.repeat(LIVE_V2_LIMITS.frameBytes + 1))).toMatchObject({
            ok: false,
            error: { code: 'limit-exceeded' },
        });
        expect(
            decode(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        order: Array.from({ length: LIVE_V2_LIMITS.operations + 1 }, (_, index) =>
                            String(index),
                        ),
                    },
                }),
            ),
        ).toMatchObject({ ok: false, error: { code: 'limit-exceeded' } });

        expect(
            decode(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        upsert: [
                            {
                                key: 'é'.repeat(LIVE_V2_LIMITS.keyLength),
                                row: '<tr></tr>',
                            },
                        ],
                    },
                }),
            ),
        ).toMatchObject({ ok: false, error: { code: 'limit-exceeded' } });
    });

    test('uses separate snapshot, delta-frame, fragment, and aggregate HTML caps', () => {
        const snapshotHTML = 's'.repeat(LIVE_V2_LIMITS.deltaBytes + 1);
        expect(
            decode({
                v: 2,
                kind: 'snapshot',
                g: 'g',
                seq: 1,
                screen: '/pods',
                rev: 'r1',
                schema: 'schema-a',
                snapshot: { html: snapshotHTML },
            }).ok,
        ).toBe(true);

        const oversizedFragment = 'x'.repeat(LIVE_V2_LIMITS.fragmentBytes + 1);
        expect(
            decode(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        upsert: [{ key: 'a', row: oversizedFragment }],
                    },
                }),
            ),
        ).toMatchObject({ ok: false, error: { code: 'limit-exceeded' } });

        const nearFragmentCap = 'x'.repeat(LIVE_V2_LIMITS.fragmentBytes - 64);
        expect(
            decode(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        upsert: [{ key: 'a', row: nearFragmentCap, card: nearFragmentCap }],
                    },
                }),
            ),
        ).toMatchObject({ ok: false, error: { code: 'limit-exceeded' } });
    });

    test.each([
        {
            name: 'missing operations',
            body: { base: 'rev-1', rev: 'rev-2' },
        },
        {
            name: 'only empty operation arrays',
            body: {
                base: 'rev-1',
                rev: 'rev-2',
                remove: [],
                upsert: [],
                order: [],
                regions: [],
            },
        },
        {
            name: 'unchanged revision',
            body: {
                base: 'rev-2',
                rev: 'rev-2',
                regions: [
                    {
                        region: 'count',
                        html: '<span class="ro-count" data-ro-live-region="count">1</span>',
                    },
                ],
            },
        },
    ])('rejects semantic no-op delta: $name', ({ body }) => {
        expect(decode(delta({ delta: body }))).toMatchObject({
            ok: false,
            error: { code: 'no-op', fatal: false },
        });
    });

    test('requires non-empty schema on snapshot/delta while terminal stays flexible', () => {
        expect(decode(delta()).ok).toBe(true);
        const withoutSchema = delta();
        delete withoutSchema.schema;
        expect(decode(withoutSchema)).toMatchObject({
            ok: false,
            error: { code: 'invalid-field' },
        });
        expect(decode(delta({ schema: '' }))).toMatchObject({
            ok: false,
            error: { code: 'invalid-field' },
        });
        expect(
            decode({
                v: 2,
                kind: 'terminal',
                g: 'g',
                seq: 2,
                screen: '/pods',
                reason: 'idle',
            }).ok,
        ).toBe(true);
    });
});
