// live-sse.ts -- bounded, byte-oriented Server-Sent Events framing.
//
// The Live transport cannot use a string accumulator: UTF-8 code points and
// CRLF delimiters may be split at arbitrary ReadableStream chunk boundaries,
// and an unterminated line must not be allowed to grow without a ceiling.  This
// parser retains byte slices until a complete line, decodes that line with a
// fatal UTF-8 decoder, and dispatches only events terminated by a blank line.

export const LIVE_SSE_LIMITS = Object.freeze({
    dataBytes: 16 * 1024 * 1024,
    // All nonblank field/comment bytes in one uncommitted event. This prevents
    // ignored extension/comment lines from multiplying bounded data work.
    eventBytes: 16 * 1024 * 1024 + 1024,
    eventNameBytes: 64,
    lines: 32,
    // A legal max-size one-line payload still carries `data:` plus optional
    // whitespace. Keep a small, fixed framing allowance separate from the
    // exact aggregate data ceiling.
    lineBytes: 16 * 1024 * 1024 + 1024,
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

    constructor(code: LiveSSEErrorCode, message: string) {
        super(message);
        this.name = 'LiveSSEError';
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
        throw new LiveSSEError('invalid-utf8', 'SSE field line is not valid UTF-8');
    }
}

export class LiveSSEParser {
    readonly #limits: SSELimits;
    #lineBuffer = new Uint8Array(1024);
    #lineBytes = 0;
    #swallowLF = false;
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
            this.#fatal =
                error instanceof LiveSSEError
                    ? error
                    : new LiveSSEError('invalid-utf8', 'SSE framing failed');
            throw this.#fatal;
        }
    }

    // EOF deliberately drops an unterminated line/event.  The caller decides
    // whether an EOF is a transport failure; the parser never invents a final
    // event without the protocol's blank-line commit marker.
    finish(): void {
        if (this.#fatal) throw this.#fatal;
        this.#lineBytes = 0;
        this.#resetEvent();
    }

    #consume(chunk: Uint8Array, events: LiveSSEEvent[]): void {
        let start = 0;
        for (let index = 0; index < chunk.byteLength; index += 1) {
            const byte = chunk[index];
            if (this.#swallowLF) {
                this.#swallowLF = false;
                if (byte === 0x0a) {
                    start = index + 1;
                    continue;
                }
            }
            if (byte !== 0x0a && byte !== 0x0d) continue;
            this.#appendLinePart(chunk.subarray(start, index));
            this.#completeLine(events);
            this.#swallowLF = byte === 0x0d;
            start = index + 1;
        }
        this.#appendLinePart(chunk.subarray(start));
    }

    #appendLinePart(part: Uint8Array): void {
        if (part.byteLength === 0) return;
        if (part.byteLength > this.#limits.lineBytes - this.#lineBytes) {
            throw new LiveSSEError('line-too-large', 'SSE field line exceeds its byte limit');
        }
        const required = this.#lineBytes + part.byteLength;
        if (required > this.#lineBuffer.byteLength) {
            let capacity = this.#lineBuffer.byteLength;
            while (capacity < required) capacity = Math.min(this.#limits.lineBytes, capacity * 2);
            const grown = new Uint8Array(capacity);
            grown.set(this.#lineBuffer.subarray(0, this.#lineBytes));
            this.#lineBuffer = grown;
        }
        this.#lineBuffer.set(part, this.#lineBytes);
        this.#lineBytes += part.byteLength;
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
            throw new LiveSSEError('event-too-large', 'SSE event framing exceeds its byte limit');
        }
        this.#eventBytes += bytes.byteLength;
        this.#lines += 1;
        if (this.#lines > this.#limits.lines) {
            throw new LiveSSEError('too-many-lines', 'SSE event has too many nonblank lines');
        }
        // A comment has no field/value split, so validate it once as a whole.
        // Other lines decode the field and value exactly once each below: this
        // validates every byte without allocating a second 16 MiB data string.
        if (bytes[0] === 0x3a) {
            decodeLine(bytes);
            return;
        }
        let colon = bytes.indexOf(0x3a);
        if (colon < 0) colon = bytes.byteLength;
        let valueStart = Math.min(colon + 1, bytes.byteLength);
        if (colon < bytes.byteLength && bytes[valueStart] === 0x20) valueStart += 1;
        const field = decodeLine(bytes.subarray(0, colon));
        const valueBytes = bytes.subarray(valueStart);
        if (field === 'event') {
            if (valueBytes.byteLength > this.#limits.eventNameBytes) {
                throw new LiveSSEError(
                    'event-name-too-large',
                    'SSE event name exceeds its byte limit',
                );
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
            throw new LiveSSEError('data-too-large', 'SSE event data exceeds its byte limit');
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
