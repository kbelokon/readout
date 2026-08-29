// live-sse.ts -- bounded, byte-oriented Server-Sent Events framing.
//
// The Live transport cannot use a string accumulator: UTF-8 code points and
// CRLF delimiters may be split at arbitrary ReadableStream chunk boundaries,
// and an unterminated line must not be allowed to grow without a ceiling.  This
// parser retains byte slices until a complete line, decodes that line with a
// fatal UTF-8 decoder, and dispatches only events terminated by a blank line.

import { ensureBoundedByteBufferCapacity } from './bounded-byte-buffer.js';

const LIVE_SSE_DATA_CEILING_BYTES = 16_777_216;
const LIVE_SSE_FRAMED_CEILING_BYTES = 16_778_240;

export const LIVE_SSE_LIMITS = Object.freeze({
    dataBytes: LIVE_SSE_DATA_CEILING_BYTES,
    // All nonblank field/comment bytes in one uncommitted event. This prevents
    // ignored extension/comment lines from multiplying bounded data work.
    eventBytes: LIVE_SSE_FRAMED_CEILING_BYTES,
    eventNameBytes: 64,
    lines: 32,
    // A legal max-size one-line payload still carries `data:` plus optional
    // whitespace. Keep a small, fixed framing allowance separate from the
    // exact aggregate data ceiling.
    lineBytes: LIVE_SSE_FRAMED_CEILING_BYTES,
});

export type LiveSSEErrorCode =
    | 'invalid-utf8'
    | 'line-too-large'
    | 'too-many-lines'
    | 'event-name-too-large'
    | 'event-too-large'
    | 'data-too-large';

export class LiveSSEError extends Error {
    readonly code: LiveSSEErrorCode;

    constructor(code: LiveSSEErrorCode) {
        super(code);
        this.code = code;
    }
}

export interface LiveSSEEvent {
    name: string | null;
    data: string;
    dataBytes: number;
}

interface SSELimits {
    dataBytes: number;
    eventBytes: number;
    eventNameBytes: number;
    lines: number;
    lineBytes: number;
}

const fatalUTF8 = new TextDecoder('utf-8', { fatal: true });

function decodeLine(bytes: Uint8Array): string {
    try {
        return fatalUTF8.decode(bytes);
    } catch {
        throw new LiveSSEError('invalid-utf8');
    }
}

export class LiveSSEParser {
    readonly #limits: SSELimits;
    #lineBuffer = new Uint8Array();
    #lineBytes = 0;
    #pendingDelimiter: 'none' | 'cr' = 'none';
    #eventName: string | null = null;
    #eventBytes = 0;
    #dataLines: string[] = [];
    #dataBytes = 0;
    #lines = 0;
    #fatal: LiveSSEError | null = null;

    constructor(limits: Partial<SSELimits> = {}) {
        this.#limits = { ...LIVE_SSE_LIMITS, ...limits };
    }

    push(chunk: Uint8Array): LiveSSEEvent[] {
        if (this.#fatal) throw this.#fatal;
        const events: LiveSSEEvent[] = [];
        try {
            this.#consume(chunk, events);
            return events;
        } catch (error) {
            this.#fatal = error instanceof LiveSSEError ? error : new LiveSSEError('invalid-utf8');
            throw this.#fatal;
        }
    }

    #consume(chunk: Uint8Array, events: LiveSSEEvent[]): void {
        let start = 0;
        // An out-of-range Uint8Array read is the loop sentinel. Keep advancing
        // the index and loading its byte together in the loop header.
        for (
            let index = 0, byte = chunk[index];
            byte !== undefined;
            index += 1, byte = chunk[index]
        ) {
            if (this.#pendingDelimiter === 'cr') {
                this.#pendingDelimiter = 'none';
                if (byte === 0x0a) {
                    start = index + 1;
                    continue;
                }
            }
            if (byte !== 0x0a && byte !== 0x0d) continue;
            this.#appendLinePart(chunk.subarray(start, index));
            this.#completeLine(events);
            this.#pendingDelimiter = byte === 0x0d ? 'cr' : 'none';
            start = index + 1;
        }
        this.#appendLinePart(chunk.subarray(start));
    }

    #appendLinePart(part: Uint8Array): void {
        if (part.byteLength > this.#limits.lineBytes - this.#lineBytes) {
            throw new LiveSSEError('line-too-large');
        }
        const required = this.#lineBytes + part.byteLength;
        this.#lineBuffer = ensureBoundedByteBufferCapacity(
            this.#lineBuffer,
            this.#lineBytes,
            part.byteLength,
            this.#limits.lineBytes,
        );
        this.#lineBuffer.set(part, this.#lineBytes);
        this.#lineBytes = required;
    }

    #completeLine(events: LiveSSEEvent[]): void {
        const bytes = this.#lineBuffer.subarray(0, this.#lineBytes);
        this.#lineBytes = 0;
        if (bytes.byteLength === 0) {
            if (this.#dataLines.length > 0) {
                events.push({
                    name: this.#eventName,
                    data: this.#dataLines.join('\n'),
                    dataBytes: this.#dataBytes,
                });
            }
            this.#resetEvent();
            return;
        }
        if (bytes.byteLength > this.#limits.eventBytes - this.#eventBytes) {
            throw new LiveSSEError('event-too-large');
        }
        this.#eventBytes += bytes.byteLength;
        this.#lines += 1;
        if (this.#lines > this.#limits.lines) {
            throw new LiveSSEError('too-many-lines');
        }
        // Decode the field and value exactly once each. A comment naturally has
        // an empty field and follows the same validated extension-field path.
        const colon = bytes.indexOf(0x3a);
        const fieldEnd = colon === -1 ? bytes.byteLength : colon;
        let valueStart = colon === -1 ? bytes.byteLength : colon + 1;
        if (bytes[valueStart] === 0x20) valueStart += 1;
        const field = decodeLine(bytes.subarray(0, fieldEnd));
        const valueBytes = bytes.subarray(valueStart);
        if (field === 'event') {
            if (valueBytes.byteLength > this.#limits.eventNameBytes) {
                throw new LiveSSEError('event-name-too-large');
            }
            this.#eventName = decodeLine(valueBytes);
            return;
        }
        if (field !== 'data') {
            decodeLine(valueBytes); // extension field: ignored only after validation
            return;
        }
        const joinedBytes =
            this.#dataBytes + valueBytes.byteLength + (this.#dataLines.length ? 1 : 0);
        if (joinedBytes > this.#limits.dataBytes) {
            throw new LiveSSEError('data-too-large');
        }
        this.#dataLines.push(decodeLine(valueBytes));
        this.#dataBytes = joinedBytes;
    }

    #resetEvent(): void {
        this.#eventName = null;
        this.#eventBytes = 0;
        this.#dataLines = [];
        this.#dataBytes = 0;
        this.#lines = 0;
    }
}
