// @vitest-environment jsdom

import { describe, expect, test, vi } from 'vitest';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    LIVE_V2_LIMITS,
    type LiveV2DeltaEnvelope,
    type LiveV2ErrorCode,
} from './live-protocol.js';

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

function expectDecodeError(value: unknown, code: LiveV2ErrorCode, message: string): void {
    expect(decode(value)).toStrictEqual({
        ok: false,
        error: { code, message, fatal: false },
    });
}

function expectFrameError(frame: string, code: LiveV2ErrorCode, message: string): void {
    expect(decodeLiveV2Envelope(frame)).toStrictEqual({
        ok: false,
        error: { code, message, fatal: false },
    });
}

function asciiPaddedFrame(value: Record<string, unknown>, targetLength: number): string {
    const empty = JSON.stringify({ ...value, padding: '' });
    return JSON.stringify({ ...value, padding: 'x'.repeat(targetLength - empty.length) });
}

function utf8PaddedFrame(value: Record<string, unknown>, targetBytes: number): string {
    const empty = JSON.stringify({ ...value, padding: '' });
    const remaining = targetBytes - new TextEncoder().encode(empty).byteLength;
    return JSON.stringify({
        ...value,
        padding: `${'é'.repeat(Math.floor(remaining / 2))}${remaining % 2 === 0 ? '' : 'x'}`,
    });
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
            name: 'top level',
            raw: '{"v":2,"v":2,"kind":"terminal","g":"g","seq":1,"screen":"/pods","reason":"idle"}',
        },
        {
            name: 'escaped-equivalent member name',
            raw: '{"v":2,"kind":"delta","g":"g","seq":2,"screen":"/pods","r\\u0065v":"r2","rev":"r2","schema":"s","delta":{"base":"r1","rev":"r2","regions":[{"region":"count","html":"<span></span>"}]}}',
        },
        {
            name: 'member name containing an escaped quote and backslash',
            raw: '{"a\\"b\\\\c":1,"a\\"b\\\\c":2}',
        },
        {
            name: 'empty member name',
            raw: '{"":1,"":2}',
        },
        {
            name: 'object nested in an array',
            raw: '{"v":2,"kind":"delta","g":"g","seq":2,"screen":"/pods","rev":"r2","schema":"s","delta":{"base":"r1","rev":"r2","remove":[{"key":"a","key":"b","cause":"delete"}]}}',
        },
        {
            name: 'after every JSON value shape',
            raw: '{"padding":[{},[],true,false,null,-1.5e2,"escaped\\"{}[]"], "v":2, "v":2}',
        },
    ])('detects duplicate members at $name', ({ raw }) => {
        expectFrameError(raw, 'duplicate', 'JSON object member names must be unique');
    });

    test('detects an escaped-equivalent duplicate after a modest long member name', () => {
        const prefix = 'x'.repeat(16 * 1024);
        const raw = `{${JSON.stringify(`${prefix}e`)}:1,"${prefix}\\u0065":2}`;

        expectFrameError(raw, 'duplicate', 'JSON object member names must be unique');
    });

    test('reports malformed JSON before considering an earlier duplicate member', () => {
        expectFrameError('{"v":2,"v":2,', 'invalid-frame', 'frame is not valid JSON');
    });

    test('enforces control-character boundaries after JSON unescaping', () => {
        for (const code of [0x00, 0x1f, 0x7f, 0x9f]) {
            expectDecodeError(
                delta({ screen: `/pods${String.fromCodePoint(code)}` }),
                'invalid-field',
                'screen contains forbidden characters',
            );
        }
        for (const code of [0x20, 0x7e, 0xa0]) {
            expect(decode(delta({ screen: `/pods${String.fromCodePoint(code)}` })).ok).toBe(true);
        }
    });

    test('accepts exactly the ASCII generation grammar', () => {
        expect(decode(delta({ g: '09AZaz._~-' })).ok).toBe(true);
        for (const generation of ['/0', '9:', '@A', 'Z[', '`a', 'z{', '!g', 'g!', 'gé']) {
            expectDecodeError(
                delta({ g: generation }),
                'invalid-field',
                'g contains forbidden characters',
            );
        }
    });

    test('accepts exact string byte boundaries and rejects the first byte beyond them', () => {
        const exactASCII = 's'.repeat(LIVE_V2_LIMITS.screenLength);
        const exactUTF8 = 'é'.repeat(LIVE_V2_LIMITS.screenLength / 2);
        expect(decode(delta({ screen: exactASCII })).ok).toBe(true);
        expect(decode(delta({ screen: exactUTF8 })).ok).toBe(true);
        expectDecodeError(
            delta({ screen: `${exactASCII}x` }),
            'limit-exceeded',
            `screen exceeds ${LIVE_V2_LIMITS.screenLength} bytes`,
        );
        expectDecodeError(
            delta({ screen: `${exactUTF8}x` }),
            'limit-exceeded',
            `screen exceeds ${LIVE_V2_LIMITS.screenLength} bytes`,
        );
    });

    test('rejects an oversized screen before encoding that nested string', () => {
        const screen = 's'.repeat(LIVE_V2_LIMITS.screenLength + 1);
        const frame = JSON.stringify(delta({ screen }));
        const encode = vi.spyOn(TextEncoder.prototype, 'encode');
        try {
            expectFrameError(
                frame,
                'limit-exceeded',
                `screen exceeds ${LIVE_V2_LIMITS.screenLength} bytes`,
            );
            expect(encode).toHaveBeenCalledWith(frame);
            expect(encode).not.toHaveBeenCalledWith(screen);
        } finally {
            encode.mockRestore();
        }
    });

    test('accepts exact fragment and operation boundaries', () => {
        const exactFragment = 'x'.repeat(LIVE_V2_LIMITS.fragmentBytes);
        expect(
            decode(
                delta({
                    delta: {
                        base: 'rev-1',
                        rev: 'rev-2',
                        upsert: [{ key: 'a', row: exactFragment }],
                    },
                }),
            ).ok,
        ).toBe(true);

        const exactOrder = Array.from({ length: LIVE_V2_LIMITS.operations }, (_, index) =>
            index.toString(36),
        );
        expect(
            decode(
                delta({
                    delta: { base: 'rev-1', rev: 'rev-2', order: exactOrder },
                }),
            ).ok,
        ).toBe(true);
    });

    test('rejects an oversized fragment before encoding that nested HTML', () => {
        const row = 'x'.repeat(LIVE_V2_LIMITS.fragmentBytes + 1);
        const frame = JSON.stringify(
            delta({
                delta: {
                    base: 'rev-1',
                    rev: 'rev-2',
                    upsert: [{ key: 'a', row }],
                },
            }),
        );
        const encode = vi.spyOn(TextEncoder.prototype, 'encode');
        try {
            expectFrameError(frame, 'limit-exceeded', 'delta.upsert[0].row exceeds the HTML limit');
            expect(encode).toHaveBeenCalledWith(frame);
            expect(encode).not.toHaveBeenCalledWith(row);
        } finally {
            encode.mockRestore();
        }
    });

    test('distinguishes code-unit and UTF-8 delta frame limits at exact boundaries', () => {
        const base = delta();
        const exactCodeUnits = asciiPaddedFrame(base, LIVE_V2_LIMITS.deltaBytes);
        const exactUTF8 = utf8PaddedFrame(base, LIVE_V2_LIMITS.deltaBytes);
        expectFrameError(exactCodeUnits, 'unexpected-field', '$.padding is not allowed');
        expectFrameError(exactUTF8, 'unexpected-field', '$.padding is not allowed');

        expectFrameError(
            asciiPaddedFrame(base, LIVE_V2_LIMITS.deltaBytes + 1),
            'limit-exceeded',
            'Live v2 frame is too large',
        );
        expectFrameError(
            utf8PaddedFrame(base, LIVE_V2_LIMITS.deltaBytes + 1),
            'limit-exceeded',
            'Live v2 frame is too large',
        );
    });

    test('rejects an oversized code-unit delta before UTF-8 encoding', () => {
        const frame = asciiPaddedFrame(delta(), LIVE_V2_LIMITS.deltaBytes + 1);
        const encode = vi.spyOn(TextEncoder.prototype, 'encode');
        try {
            expectFrameError(frame, 'limit-exceeded', 'Live v2 frame is too large');
            expect(encode).not.toHaveBeenCalled();
        } finally {
            encode.mockRestore();
        }
    });

    test('admits the exact global frame boundary before JSON validation', () => {
        // Fail at the first JSON byte after crossing the size gate. This pins
        // the production 16 MiB boundary without making instrumented mutation
        // runs scan and allocate a valid 16 MiB object graph.
        expectFrameError(
            'x'.padEnd(LIVE_V2_LIMITS.frameBytes, ' '),
            'invalid-frame',
            'frame is not valid JSON',
        );
    });

    test.each([
        {
            name: 'remove',
            body: { remove: Array.from({ length: LIVE_V2_LIMITS.operations + 1 }, () => 0) },
            message: 'delta.remove must be a bounded array',
        },
        {
            name: 'upsert',
            body: { upsert: Array.from({ length: LIVE_V2_LIMITS.operations + 1 }, () => 0) },
            message: 'delta.upsert must be a bounded array',
        },
        {
            name: 'order',
            body: { order: Array.from({ length: LIVE_V2_LIMITS.operations + 1 }, () => 0) },
            message: 'delta.order must be a bounded array',
        },
        {
            name: 'regions',
            body: { regions: Array.from({ length: 4 }, () => 0) },
            message: 'delta.regions must be a bounded array',
        },
    ])('rejects a $name collection beyond its cardinality cap', ({ body, message }) => {
        expectDecodeError(
            delta({ delta: { base: 'rev-1', rev: 'rev-2', ...body } }),
            'limit-exceeded',
            message,
        );
    });

    test('does not turn exact collection caps into off-by-one limit failures', () => {
        for (const [field, value, message] of [
            [
                'remove',
                Array.from({ length: LIVE_V2_LIMITS.operations }, () => 0),
                'delta.remove[0]',
            ],
            [
                'upsert',
                Array.from({ length: LIVE_V2_LIMITS.operations }, () => 0),
                'delta.upsert[0]',
            ],
        ] as const) {
            expectDecodeError(
                delta({ delta: { base: 'rev-1', rev: 'rev-2', [field]: value } }),
                'invalid-field',
                message,
            );
        }

        expectDecodeError(
            delta({
                delta: {
                    base: 'rev-1',
                    rev: 'rev-2',
                    regions: [0, 0, 0],
                },
            }),
            'invalid-field',
            'delta.regions[0]',
        );
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

    test('uses separate snapshot, delta-frame, and per-fragment caps', () => {
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

    test.each([
        {
            name: 'unsupported version',
            value: delta({ v: 1 }),
            code: 'unsupported-version' as const,
            message: 'v must be exactly 2',
        },
        {
            name: 'zero sequence',
            value: delta({ seq: 0 }),
            code: 'invalid-field' as const,
            message: 'seq must be a positive safe integer',
        },
        {
            name: 'empty generation',
            value: delta({ g: '' }),
            code: 'invalid-field' as const,
            message: 'g must be a non-empty string',
        },
        {
            name: 'forbidden generation',
            value: delta({ g: 'not allowed' }),
            code: 'invalid-field' as const,
            message: 'g contains forbidden characters',
        },
        {
            name: 'empty screen',
            value: delta({ screen: '' }),
            code: 'invalid-field' as const,
            message: 'screen must be a non-empty string',
        },
        {
            name: 'empty envelope revision',
            value: delta({ rev: '' }),
            code: 'invalid-field' as const,
            message: 'rev must be a non-empty string',
        },
        {
            name: 'empty resource version',
            value: delta({ rv: '' }),
            code: 'invalid-field' as const,
            message: 'rv must be a non-empty string',
        },
        {
            name: 'empty schema',
            value: delta({ schema: '' }),
            code: 'invalid-field' as const,
            message: 'schema must be a non-empty string',
        },
        {
            name: 'unknown envelope member',
            value: delta({ surprise: true }),
            code: 'unexpected-field' as const,
            message: '$.surprise is not allowed',
        },
        {
            name: 'unknown delta member',
            value: delta({ delta: { base: 'rev-1', rev: 'rev-2', surprise: true } }),
            code: 'unexpected-field' as const,
            message: 'delta.surprise is not allowed',
        },
        {
            name: 'missing delta base',
            value: delta({ delta: { rev: 'rev-2', order: ['a'] } }),
            code: 'invalid-field' as const,
            message: 'delta.base must be a non-empty string',
        },
        {
            name: 'missing delta revision',
            value: delta({ delta: { base: 'rev-1', order: ['a'] } }),
            code: 'invalid-field' as const,
            message: 'delta.rev must be a non-empty string',
        },
        {
            name: 'envelope and delta revision disagreement',
            value: delta({ delta: { base: 'rev-1', rev: 'other', order: ['a'] } }),
            code: 'invalid-field' as const,
            message: 'envelope.rev must equal delta.rev',
        },
        {
            name: 'equal base and revision',
            value: delta({ delta: { base: 'rev-2', rev: 'rev-2', order: ['a'] } }),
            code: 'no-op' as const,
            message: 'delta base and revision must differ',
        },
        {
            name: 'empty semantic delta',
            value: delta({ delta: { base: 'rev-1', rev: 'rev-2' } }),
            code: 'no-op' as const,
            message: 'delta has no semantic operations',
        },
    ])('keeps the diagnostic contract for $name', ({ value, code, message }) => {
        expectDecodeError(value, code, message);
    });

    test('requires each snapshot structural field independently', () => {
        const snapshot = {
            v: 2,
            kind: 'snapshot',
            g: 'g',
            seq: 1,
            screen: '/pods',
            rev: 'r1',
            schema: 's',
            snapshot: { html: '<div></div>' },
        };
        for (const field of ['rev', 'schema'] as const) {
            const missing = { ...snapshot };
            delete missing[field];
            expectDecodeError(missing, 'invalid-field', 'snapshot rev and schema are required');
        }
        expectDecodeError(
            { ...snapshot, snapshot: null },
            'invalid-field',
            'snapshot must be an object',
        );
        expectDecodeError(
            { ...snapshot, snapshot: { html: '<div></div>', extra: true } },
            'unexpected-field',
            'snapshot.extra is not allowed',
        );
        expectDecodeError(
            { ...snapshot, snapshot: { html: '' } },
            'invalid-field',
            'snapshot.html must be non-empty HTML',
        );
    });

    test('requires each delta structural field independently', () => {
        const complete = delta();
        for (const field of ['rev', 'schema', 'delta'] as const) {
            const missing = { ...complete };
            delete missing[field];
            expectDecodeError(
                missing,
                'invalid-field',
                'delta, envelope rev, and schema are required',
            );
        }
        expectDecodeError(
            { ...complete, delta: [] },
            'invalid-field',
            'delta, envelope rev, and schema are required',
        );
    });

    test.each([
        {
            name: 'remove is not an array',
            body: { remove: {} },
            code: 'invalid-field' as const,
            message: 'delta.remove must be a bounded array',
        },
        {
            name: 'remove item is not an object',
            body: { remove: [null] },
            code: 'invalid-field' as const,
            message: 'delta.remove[0]',
        },
        {
            name: 'remove item has an unknown member',
            body: { remove: [{ key: 'a', cause: 'delete', extra: true }] },
            code: 'unexpected-field' as const,
            message: 'delta.remove[0].extra is not allowed',
        },
        {
            name: 'remove key is empty',
            body: { remove: [{ key: '', cause: 'delete' }] },
            code: 'invalid-field' as const,
            message: 'delta.remove[0].key must be a non-empty string',
        },
        {
            name: 'remove cause is unknown',
            body: { remove: [{ key: 'a', cause: 'gone' }] },
            code: 'invalid-field' as const,
            message: 'delta.remove[0].cause is unknown',
        },
        {
            name: 'remove key repeats',
            body: {
                remove: [
                    { key: 'a', cause: 'delete' },
                    { key: 'a', cause: 'project' },
                ],
            },
            code: 'duplicate' as const,
            message: 'duplicate remove key a',
        },
        {
            name: 'upsert is not an array',
            body: { upsert: {} },
            code: 'invalid-field' as const,
            message: 'delta.upsert must be a bounded array',
        },
        {
            name: 'upsert item is not an object',
            body: { upsert: [null] },
            code: 'invalid-field' as const,
            message: 'delta.upsert[0]',
        },
        {
            name: 'upsert item has an unknown member',
            body: { upsert: [{ key: 'a', row: '<tr></tr>', extra: true }] },
            code: 'unexpected-field' as const,
            message: 'delta.upsert[0].extra is not allowed',
        },
        {
            name: 'upsert row is empty',
            body: { upsert: [{ key: 'a', row: '' }] },
            code: 'invalid-field' as const,
            message: 'delta.upsert[0].row must be non-empty HTML',
        },
        {
            name: 'upsert key is empty',
            body: { upsert: [{ key: '', row: '<tr></tr>' }] },
            code: 'invalid-field' as const,
            message: 'delta.upsert[0].key must be a non-empty string',
        },
        {
            name: 'upsert card is empty',
            body: { upsert: [{ key: 'a', row: '<tr></tr>', card: '' }] },
            code: 'invalid-field' as const,
            message: 'delta.upsert[0].card must be non-empty HTML',
        },
        {
            name: 'upsert key repeats',
            body: {
                upsert: [
                    { key: 'a', row: '<tr></tr>' },
                    { key: 'a', row: '<tr></tr>' },
                ],
            },
            code: 'duplicate' as const,
            message: 'duplicate upsert key a',
        },
        {
            name: 'order is not an array',
            body: { order: {} },
            code: 'invalid-field' as const,
            message: 'delta.order must be a bounded array',
        },
        {
            name: 'order key repeats',
            body: { order: ['a', 'a'] },
            code: 'duplicate' as const,
            message: 'duplicate order key a',
        },
        {
            name: 'order key is empty',
            body: { order: [''] },
            code: 'invalid-field' as const,
            message: 'delta.order[0] must be a non-empty string',
        },
        {
            name: 'regions is not an array',
            body: { regions: {} },
            code: 'invalid-field' as const,
            message: 'delta.regions must be a bounded array',
        },
        {
            name: 'region item is not an object',
            body: { regions: [null] },
            code: 'invalid-field' as const,
            message: 'delta.regions[0]',
        },
        {
            name: 'region item has an unknown member',
            body: { regions: [{ region: 'count', html: '<span></span>', extra: true }] },
            code: 'unexpected-field' as const,
            message: 'delta.regions[0].extra is not allowed',
        },
        {
            name: 'region name is unknown',
            body: { regions: [{ region: 'other', html: '<span></span>' }] },
            code: 'invalid-field' as const,
            message: 'delta.regions[0].region is unknown',
        },
        {
            name: 'region HTML is empty',
            body: { regions: [{ region: 'count', html: '' }] },
            code: 'invalid-field' as const,
            message: 'delta.regions[0].html must be non-empty HTML',
        },
        {
            name: 'region repeats',
            body: {
                regions: [
                    { region: 'count', html: '<span>1</span>' },
                    { region: 'count', html: '<span>2</span>' },
                ],
            },
            code: 'duplicate' as const,
            message: 'duplicate region count',
        },
        {
            name: 'remove and upsert overlap',
            body: {
                remove: [{ key: 'a', cause: 'delete' }],
                upsert: [{ key: 'a', row: '<tr></tr>' }],
            },
            code: 'duplicate' as const,
            message: 'key a is removed and upserted',
        },
    ])('keeps the operation diagnostic for $name', ({ body, code, message }) => {
        expectDecodeError(
            delta({ delta: { base: 'rev-1', rev: 'rev-2', ...body } }),
            code,
            message,
        );
    });

    test('accepts every remove, region, and terminal enum member', () => {
        for (const cause of ['delete', 'project'] as const) {
            expect(
                decode(
                    delta({
                        delta: {
                            base: 'rev-1',
                            rev: 'rev-2',
                            remove: [{ key: cause, cause }],
                        },
                    }),
                ).ok,
            ).toBe(true);
        }
        for (const region of ['count', 'phase', 'found'] as const) {
            expect(
                decode(
                    delta({
                        delta: {
                            base: 'rev-1',
                            rev: 'rev-2',
                            regions: [{ region, html: '<span></span>' }],
                        },
                    }),
                ).ok,
            ).toBe(true);
        }
        for (const reason of ['idle', 'auth', 'watch-failed', 'shutdown'] as const) {
            expect(
                decode({
                    v: 2,
                    kind: 'terminal',
                    g: 'g',
                    seq: 1,
                    screen: '/pods',
                    reason,
                }).ok,
            ).toBe(true);
        }
        expectDecodeError(
            {
                v: 2,
                kind: 'terminal',
                g: 'g',
                seq: 1,
                screen: '/pods',
                reason: 'retry',
            },
            'invalid-field',
            'terminal reason is unknown',
        );
    });

    test('distinguishes root, kind, and raw-frame failures exactly', () => {
        expectFrameError('[]', 'invalid-frame', 'frame root must be an object');
        expectDecodeError(delta({ kind: 'other' }), 'unknown-kind', 'kind is unknown');
        expectFrameError('{', 'invalid-frame', 'frame is not valid JSON');
        expect(decodeLiveV2Envelope(null as unknown as string)).toStrictEqual({
            ok: false,
            error: {
                code: 'limit-exceeded',
                message: 'Live v2 frame is too large',
                fatal: false,
            },
        });
    });

    test('reports an exact fragment byte overflow diagnostic', () => {
        expectDecodeError(
            delta({
                delta: {
                    base: 'rev-1',
                    rev: 'rev-2',
                    upsert: [
                        {
                            key: 'a',
                            row: 'é'.repeat(LIVE_V2_LIMITS.fragmentBytes / 2 + 1),
                        },
                    ],
                },
            }),
            'limit-exceeded',
            'delta.upsert[0].row exceeds the HTML limit',
        );
    });

    test('rejects decoded non-delta capabilities before touching projection state', () => {
        const snapshot = decode({
            v: 2,
            kind: 'snapshot',
            g: 'g',
            seq: 1,
            screen: '/pods',
            rev: 'r1',
            schema: 's',
            snapshot: { html: '<div></div>' },
        });
        expect(snapshot.ok).toBe(true);
        if (!snapshot.ok) return;
        expect(
            applyLiveV2Delta(snapshot.value, {
                g: 'g',
                seq: 1,
                screen: '/pods',
                rev: 'r1',
                schema: 's',
            }),
        ).toStrictEqual({
            ok: false,
            error: {
                code: 'not-delta',
                message: 'only delta envelopes can be applied',
                fatal: false,
            },
        });
    });

    test('rejects an untrusted apply input with the exact capability diagnostic', () => {
        expect(
            applyLiveV2Delta(null, {
                g: 'g',
                seq: 1,
                screen: '/pods',
                rev: 'r1',
                schema: 's',
            }),
        ).toStrictEqual({
            ok: false,
            error: {
                code: 'invalid-frame',
                message: 'Live v2 apply input must be a raw frame or an opaque decoder result',
                fatal: false,
            },
        });
    });
});
