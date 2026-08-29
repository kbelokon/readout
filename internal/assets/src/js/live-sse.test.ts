import { describe, expect, test } from 'vitest';

import { LIVE_SSE_LIMITS, LiveSSEError, LiveSSEParser } from './live-sse.js';

const encoder = new TextEncoder();

function parse(parts: Array<string | Uint8Array>, limits = {}) {
    const parser = new LiveSSEParser(limits);
    const events = parts.flatMap((part) =>
        parser.push(typeof part === 'string' ? encoder.encode(part) : part),
    );
    return events;
}

describe('LiveSSEParser', () => {
    test('pins the exact production framing allowances without parsing large payloads', () => {
        expect(LIVE_SSE_LIMITS.eventBytes).toBe(LIVE_SSE_LIMITS.dataBytes + 1024);
        expect(LIVE_SSE_LIMITS.lineBytes).toBe(LIVE_SSE_LIMITS.dataBytes + 1024);
    });

    test('publishes the intended production ceilings', () => {
        expect(LIVE_SSE_LIMITS).toStrictEqual({
            dataBytes: 16_777_216,
            eventBytes: 16_778_240,
            eventNameBytes: 64,
            lines: 32,
            lineBytes: 16_778_240,
        });
    });

    test('preserves a retained line across one small asymmetric growth step', () => {
        const parser = new LiveSSEParser({
            dataBytes: 2_124,
            eventBytes: 2_133,
            lineBytes: 2_124,
        });
        const retained = `data:${'x'.repeat(1_019)}`;
        const appended = 'y'.repeat(1_100);
        const copiedData = `${'x'.repeat(1_019)}${appended}`;
        const data = `${copiedData}\nzzzz`;

        expect(encoder.encode(retained).byteLength).toBe(1_024);
        expect(parser.push(encoder.encode(retained))).toStrictEqual([]);
        expect(parser.push(encoder.encode(appended))).toStrictEqual([]);
        expect(encoder.encode(`${retained}${appended}`).byteLength).toBe(2_124);

        expect(parser.push(encoder.encode('\ndata:zzzz\n\n'))).toStrictEqual([
            { name: null, data, dataBytes: 2_124 },
        ]);
    });

    test('parses every byte boundary including a split UTF-8 code point', () => {
        const bytes = encoder.encode(': heartbeat\r\nevent: ro-live\rdata: {"emoji":"🫠"}\r\r');
        const parser = new LiveSSEParser();
        const events = Array.from(bytes).flatMap((byte) => parser.push(Uint8Array.of(byte)));

        expect(events).toStrictEqual([
            {
                name: 'ro-live',
                data: '{"emoji":"🫠"}',
                dataBytes: encoder.encode('{"emoji":"🫠"}').byteLength,
            },
        ]);
    });

    test.each([
        ['LF', '\n'],
        ['CRLF', '\r\n'],
        ['bare CR', '\r'],
    ])('accepts %s delimiters', (_name, newline) => {
        expect(
            parse([`event: x${newline}data: one${newline}data:two${newline}${newline}`]),
        ).toStrictEqual([{ name: 'x', data: 'one\ntwo', dataBytes: 7 }]);
    });

    test('removes exactly one optional ASCII space and ignores comments/unknown fields', () => {
        expect(
            parse([':data: ignored\nretry: 7\nevent:  ro-live\ndata:  first\ndata:second\n\n']),
        ).toStrictEqual([{ name: ' ro-live', data: ' first\nsecond', dataBytes: 13 }]);
    });

    test('dispatches back-to-back committed events but drops a partial EOF event', () => {
        expect(parse(['data:a\n\ndata:b\n\ndata:not-committed'])).toStrictEqual([
            { name: null, data: 'a', dataBytes: 1 },
            { name: null, data: 'b', dataBytes: 1 },
        ]);
    });

    test('does not dispatch field-only events and accepts a data field without a colon', () => {
        expect(parse(['event:x\nretry:1\n\n'])).toStrictEqual([]);
        expect(parse(['data\n\n'])).toStrictEqual([{ name: null, data: '', dataBytes: 0 }]);
    });

    test('accepts each exact cap', () => {
        const parser = new LiveSSEParser({
            dataBytes: 3,
            eventNameBytes: 2,
            lines: 2,
            lineBytes: 8,
        });
        expect(parser.push(encoder.encode('event:xy\ndata:abc\n\n'))).toStrictEqual([
            { name: 'xy', data: 'abc', dataBytes: 3 },
        ]);
    });

    test.each([
        ['event-name-too-large', () => parse(['event:abc\ndata:x\n\n'], { eventNameBytes: 2 })],
        ['too-many-lines', () => parse(['x:1\ny:2\ndata:z\n\n'], { lines: 2 })],
        ['data-too-large', () => parse(['data:ab\ndata:c\n\n'], { dataBytes: 3 })],
        ['event-too-large', () => parse([':1234\nx:5678\n'], { eventBytes: 10 })],
        ['line-too-large', () => parse(['data:abcd'], { lineBytes: 8 })],
    ] as const)('fails fatally at cap + 1: %s', (code, run) => {
        expect(run).toThrowError(expect.objectContaining({ code }));
    });

    test('bounds an endless unterminated line before EOF', () => {
        const parser = new LiveSSEParser({ lineBytes: 4 });
        parser.push(encoder.encode('1234'));
        expect(() => parser.push(encoder.encode('5'))).toThrowError(
            expect.objectContaining({ code: 'line-too-large' }),
        );
    });

    test('rejects invalid UTF-8 only once the containing line completes', () => {
        const parser = new LiveSSEParser();
        expect(parser.push(Uint8Array.of(0x64, 0x61, 0x74, 0x61, 0x3a, 0xc3))).toStrictEqual([]);
        expect(() => parser.push(Uint8Array.of(0x0a))).toThrowError(
            expect.objectContaining({
                code: 'invalid-utf8',
            }),
        );
        expect(() => parser.push(encoder.encode('data:ok\n\n'))).toThrow(LiveSSEError);
    });

    test.each([Uint8Array.of(0x3a, 0xc3, 0x0a), Uint8Array.of(0x78, 0x3a, 0xc3, 0x0a)])(
        'validates UTF-8 in ignored comments and extension fields',
        (line) => {
            expect(() => new LiveSSEParser().push(line)).toThrowError(
                expect.objectContaining({ code: 'invalid-utf8' }),
            );
        },
    );

    test('retains bounded framing across thousands of one-byte chunks', () => {
        const parser = new LiveSSEParser({ lineBytes: 20_000 });
        for (const byte of encoder.encode(`data:${'x'.repeat(10_000)}\n`)) {
            parser.push(Uint8Array.of(byte));
        }
        expect(parser.push(encoder.encode('\n'))).toStrictEqual([
            { name: null, data: 'x'.repeat(10_000), dataBytes: 10_000 },
        ]);
    });

    test('accepts an exact geometric line cap and rejects the next byte', () => {
        const line = `data:${'x'.repeat(1_095)}`;
        const retained = encoder.encode(line.slice(0, 1_024));
        const remainder = encoder.encode(line.slice(1_024));
        expect(retained.byteLength).toBe(1_024);
        expect(remainder.byteLength).toBe(76);

        const exact = new LiveSSEParser({ lineBytes: 1_100 });
        expect(exact.push(retained)).toStrictEqual([]);
        expect(exact.push(remainder)).toStrictEqual([]);
        expect(exact.push(encoder.encode('\n\n'))).toStrictEqual([
            { name: null, data: 'x'.repeat(1_095), dataBytes: 1_095 },
        ]);

        const over = new LiveSSEParser({ lineBytes: 1_100 });
        over.push(retained);
        over.push(remainder);
        expect(() => over.push(encoder.encode('x'))).toThrowError(
            expect.objectContaining({ code: 'line-too-large' }),
        );
    });

    test.each([':1234\n:5678\n:90\n', 'x:1234\ny:5678\nz:9\n'])(
        'bounds aggregate ignored/comment framing before decode amplification',
        (wire) => {
            expect(() => parse([wire], { eventBytes: 12 })).toThrowError(
                expect.objectContaining({ code: 'event-too-large' }),
            );
        },
    );

    test('accepts the aggregate event-byte boundary exactly', () => {
        expect(parse(['event:x\ndata:abc\n\n'], { eventBytes: 15 })).toStrictEqual([
            { name: 'x', data: 'abc', dataBytes: 3 },
        ]);
    });

    test.each([
        {
            exact: `${Array.from({ length: 32 }, () => ':ok').join('\n')}\n\n`,
            name: 'comments',
            plusOne: `${Array.from({ length: 33 }, () => ':ok').join('\n')}\n`,
        },
        {
            exact: `${[
                ...Array.from({ length: 16 }, () => ':ok'),
                ...Array.from({ length: 15 }, (_, index) => `x${index}:ok`),
                'data:ok',
            ].join('\n')}\n\n`,
            name: 'mixed comments and fields',
            plusOne: `${[
                ...Array.from({ length: 16 }, () => ':ok'),
                ...Array.from({ length: 16 }, (_, index) => `x${index}:ok`),
                'data:ok',
            ].join('\n')}\n`,
        },
    ])('counts every nonblank $name line at exact 32 and rejects 33', ({ exact, plusOne }) => {
        expect(() => parse([exact])).not.toThrow();
        expect(() => parse([plusOne])).toThrowError(
            expect.objectContaining({ code: 'too-many-lines' }),
        );
    });
});
