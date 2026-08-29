// live-protocol.ts -- transport-neutral Live v2 wire validation and atomic
// projection application.
//
// EventSource is deliberately absent from this module.  A transport decodes a
// frame, compares/applies it against its private cursor, and only publishes the
// returned cursor after this synchronous function has reconciled every DOM and
// in-memory view.  Failed frames therefore cannot advance the stream.

import {
    applyLiveNameFilter,
    restoreLiveNameFilterCheckpoint,
    takeLiveNameFilterCheckpoint,
} from './filters.js';
import {
    applyListProjectionDeltaTransaction,
    type ListProjectionDeltaPlan,
    type ListProjectionDeltaSummary,
} from './list-projection.js';
import {
    applyLiveRowDeletions,
    reapplyRowState,
    restoreRowStateCheckpoint,
    takeRowStateCheckpoint,
} from './row-selection.js';
import {
    restoreVirtualizerCheckpoint,
    takeVirtualizerCheckpoint,
    virtualizerActive,
} from './virtualizer.js';

export const LIVE_V2_LIMITS = Object.freeze({
    frameBytes: 16 * 1024 * 1024,
    deltaBytes: 256 * 1024,
    fragmentBytes: 128 * 1024,
    generationLength: 64,
    snapshotBytes: 16 * 1024 * 1024,
    keyLength: 2 * 1024,
    operations: 20_000,
    revisionLength: 128,
    resourceVersionLength: 256,
    schemaLength: 128,
    screenLength: 8 * 1024,
});

export type LiveV2Kind = 'snapshot' | 'delta' | 'terminal';
export type LiveV2RemoveCause = 'delete' | 'project';
export type LiveV2RegionName = 'count' | 'phase' | 'found';
export type LiveV2TerminalReason = 'idle' | 'auth' | 'watch-failed' | 'shutdown';

interface LiveV2EnvelopeBase {
    v: 2;
    g: string;
    seq: number;
    screen: string;
    rev?: string;
    rv?: string;
    schema?: string;
}

export interface LiveV2SnapshotEnvelope extends LiveV2EnvelopeBase {
    kind: 'snapshot';
    rev: string;
    schema: string;
    snapshot: { html: string };
}

export interface LiveV2DeltaRemove {
    key: string;
    cause: LiveV2RemoveCause;
}

export interface LiveV2DeltaUpsert {
    key: string;
    row: string;
    card?: string;
}

export interface LiveV2DeltaRegion {
    region: LiveV2RegionName;
    html: string;
}

export interface LiveV2Delta {
    base: string;
    rev: string;
    remove?: LiveV2DeltaRemove[];
    upsert?: LiveV2DeltaUpsert[];
    order?: string[];
    regions?: LiveV2DeltaRegion[];
}

export interface LiveV2DeltaEnvelope extends LiveV2EnvelopeBase {
    kind: 'delta';
    rev: string;
    schema: string;
    delta: LiveV2Delta;
}

export interface LiveV2TerminalEnvelope extends LiveV2EnvelopeBase {
    kind: 'terminal';
    reason: LiveV2TerminalReason;
}

export type LiveV2Envelope = LiveV2SnapshotEnvelope | LiveV2DeltaEnvelope | LiveV2TerminalEnvelope;

export interface LiveV2Cursor {
    g: string;
    seq: number;
    screen: string;
    rev: string;
    rv?: string;
    schema: string;
}

export type LiveV2ErrorCode =
    | 'invalid-frame'
    | 'unsupported-version'
    | 'unknown-kind'
    | 'unexpected-field'
    | 'invalid-field'
    | 'duplicate'
    | 'no-op'
    | 'limit-exceeded'
    | 'not-delta'
    | 'generation-mismatch'
    | 'screen-mismatch'
    | 'sequence-gap'
    | 'base-mismatch'
    | 'schema-mismatch'
    | 'projection-mismatch'
    | 'fragment-invalid'
    | 'morph-failed'
    | 'reconcile-failed'
    | 'rollback-failed';

export interface LiveV2Error {
    code: LiveV2ErrorCode;
    message: string;
    fatal: boolean;
}

export type DecodeLiveV2Result =
    | { ok: true; value: LiveV2Envelope }
    | { ok: false; error: LiveV2Error };

export interface ApplyLiveV2DeltaSuccess {
    ok: true;
    cursor: LiveV2Cursor;
    summary: ListProjectionDeltaSummary;
}

export type ApplyLiveV2DeltaResult = ApplyLiveV2DeltaSuccess | { ok: false; error: LiveV2Error };

export interface LiveV2ApplyTestHooks {
    morph?: (current: HTMLElement, incoming: HTMLElement) => unknown;
    beforeReconcile?: () => void;
    afterReconcile?: () => void;
}

type JSONRecord = Record<string, unknown>;

const BASE_FIELDS = new Set(['v', 'kind', 'g', 'seq', 'screen', 'rev', 'rv', 'schema']);
const ROOT_PATH = '$';
const textEncoder = new TextEncoder();
const decodedEnvelopeTokens = new WeakSet<object>();

function wireError(code: LiveV2ErrorCode, message: string, fatal = false): LiveV2Error {
    return { code, message, fatal };
}

function isRecord(value: unknown): value is JSONRecord {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function freezeWireValue(value: unknown): void {
    if (Object(value) !== value) return;
    for (const child of Object.values(value as object)) freezeWireValue(child);
    Object.freeze(value);
}

// Decoder outputs are immutable runtime capabilities. applyLiveV2Delta accepts
// either one of these opaque objects or a raw JSON frame that it decodes itself;
// an arbitrary structural lookalike can never reach the mutation reducer.
function sealDecodedEnvelope<T extends LiveV2Envelope>(value: T): T {
    freezeWireValue(value);
    decodedEnvelopeTokens.add(value);
    return value;
}

function own(record: JSONRecord, key: string): boolean {
    return Object.hasOwn(record, key);
}

function hasControlCharacters(value: string): boolean {
    for (const character of value) {
        const code = character.codePointAt(0) as number;
        if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true;
    }
    return false;
}

function isGeneration(value: string): boolean {
    for (const character of value) {
        const code = character.charCodeAt(0);
        if (
            (code >= 0x30 && code <= 0x39) ||
            (code >= 0x41 && code <= 0x5a) ||
            (code >= 0x61 && code <= 0x7a) ||
            character === '.' ||
            character === '_' ||
            character === '~' ||
            character === '-'
        ) {
            continue;
        }
        return false;
    }
    return true;
}

// JSON.parse deliberately keeps the last value for duplicate object members.
// Once JSON.parse has established that the frame is valid JSON, this small
// grammar walk detects duplicate member names without maintaining a second
// syntax validator.
function hasDuplicateJSONMembers(source: string): boolean {
    type ObjectContext = { kind: 'object'; expectsKey: boolean; keys: Set<string> };
    type Context = { kind: 'array' } | ObjectContext;
    type ValueStringState = { kind: 'value'; mode: 'content' | 'escape' };
    type KeyStringState = {
        kind: 'key';
        mode: 'content' | 'escape';
        keyContext: ObjectContext;
        rawStart: number;
    };
    type StringState = ValueStringState | KeyStringState;

    const contexts: Context[] = [];
    let stringState: StringState | null = null;

    for (let offset = 0; offset < source.length; offset += 1) {
        const characterCode = source.charCodeAt(offset);
        if (stringState) {
            if (stringState.kind === 'value') {
                if (stringState.mode === 'escape') stringState.mode = 'content';
                else if (characterCode === 0x5c) stringState.mode = 'escape';
                else if (characterCode === 0x22) stringState = null;
                continue;
            }

            if (stringState.mode === 'escape') {
                stringState.mode = 'content';
                continue;
            }
            if (characterCode === 0x5c) {
                stringState.mode = 'escape';
                continue;
            }
            if (characterCode !== 0x22) continue;

            const { keyContext, rawStart } = stringState;
            stringState = null;
            const key = JSON.parse(source.slice(rawStart, offset + 1)) as string;
            if (keyContext.keys.has(key)) return true;
            keyContext.keys.add(key);
            keyContext.expectsKey = false;
            continue;
        }

        const context = contexts.at(-1);
        if (characterCode === 0x22) {
            stringState =
                context?.kind === 'object' && context.expectsKey
                    ? {
                          kind: 'key',
                          mode: 'content',
                          keyContext: context,
                          rawStart: offset,
                      }
                    : { kind: 'value', mode: 'content' };
            continue;
        }
        if (characterCode === 0x7b) {
            contexts.push({ kind: 'object', expectsKey: true, keys: new Set() });
            continue;
        }
        if (characterCode === 0x5b) {
            contexts.push({ kind: 'array' });
            continue;
        }
        if (characterCode === 0x7d || characterCode === 0x5d) {
            contexts.pop();
            continue;
        }
        if (characterCode === 0x2c) {
            if (context?.kind === 'object') context.expectsKey = true;
        }
    }
    return false;
}

function rejectUnknownFields(
    record: JSONRecord,
    allowed: ReadonlySet<string>,
    path: string,
): LiveV2Error | null {
    const unknown = Object.keys(record).find((key) => !allowed.has(key));
    return unknown ? wireError('unexpected-field', `${path}.${unknown} is not allowed`) : null;
}

function boundedString(value: unknown, path: string, max: number): string | LiveV2Error {
    if (typeof value !== 'string' || value.length === 0) {
        return wireError('invalid-field', `${path} must be a non-empty string`);
    }
    if (value.length > max || textEncoder.encode(value).byteLength > max) {
        return wireError('limit-exceeded', `${path} exceeds ${max} bytes`);
    }
    if (hasControlCharacters(value)) {
        return wireError('invalid-field', `${path} contains forbidden characters`);
    }
    return value;
}

function htmlString(value: unknown, path: string, maxBytes: number): string | LiveV2Error {
    if (typeof value !== 'string' || value.length === 0) {
        return wireError('invalid-field', `${path} must be non-empty HTML`);
    }
    if (value.length > maxBytes || textEncoder.encode(value).byteLength > maxBytes) {
        return wireError('limit-exceeded', `${path} exceeds the HTML limit`);
    }
    return value;
}

function decodeBase(record: JSONRecord): LiveV2Error | null {
    if (record.v !== 2) {
        return wireError('unsupported-version', 'v must be exactly 2');
    }
    if (!Number.isSafeInteger(record.seq) || (record.seq as number) < 1) {
        return wireError('invalid-field', 'seq must be a positive safe integer');
    }
    const g = boundedString(record.g, 'g', LIVE_V2_LIMITS.generationLength);
    if (typeof g !== 'string') return g;
    if (!isGeneration(g)) {
        return wireError('invalid-field', 'g contains forbidden characters');
    }
    const screen = boundedString(record.screen, 'screen', LIVE_V2_LIMITS.screenLength);
    if (typeof screen !== 'string') return screen;
    if (own(record, 'rev')) {
        const rev = boundedString(record.rev, 'rev', LIVE_V2_LIMITS.revisionLength);
        if (typeof rev !== 'string') return rev;
    }
    if (own(record, 'rv')) {
        const rv = boundedString(record.rv, 'rv', LIVE_V2_LIMITS.resourceVersionLength);
        if (typeof rv !== 'string') return rv;
    }
    if (own(record, 'schema')) {
        const schema = boundedString(record.schema, 'schema', LIVE_V2_LIMITS.schemaLength);
        if (typeof schema !== 'string') return schema;
    }
    return null;
}

function decodeSnapshot(record: JSONRecord): DecodeLiveV2Result {
    const allowed = new Set([...BASE_FIELDS, 'snapshot']);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (!own(record, 'rev') || !own(record, 'schema')) {
        return {
            ok: false,
            error: wireError('invalid-field', 'snapshot rev and schema are required'),
        };
    }
    if (!isRecord(record.snapshot)) {
        return { ok: false, error: wireError('invalid-field', 'snapshot must be an object') };
    }
    const nestedUnknown = rejectUnknownFields(record.snapshot, new Set(['html']), 'snapshot');
    if (nestedUnknown) return { ok: false, error: nestedUnknown };
    const html = htmlString(record.snapshot.html, 'snapshot.html', LIVE_V2_LIMITS.snapshotBytes);
    if (typeof html !== 'string') return { ok: false, error: html };
    return {
        ok: true,
        value: sealDecodedEnvelope(record as unknown as LiveV2SnapshotEnvelope),
    };
}

function decodeRemove(value: unknown): LiveV2DeltaRemove[] | LiveV2Error {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
        return wireError(
            Array.isArray(value) ? 'limit-exceeded' : 'invalid-field',
            'delta.remove must be a bounded array',
        );
    }
    const seen = new Set<string>();
    const result: LiveV2DeltaRemove[] = [];
    for (let index = 0; index < value.length; index += 1) {
        const item = value[index];
        if (!isRecord(item)) return wireError('invalid-field', `delta.remove[${index}]`);
        const unknown = rejectUnknownFields(
            item,
            new Set(['key', 'cause']),
            `delta.remove[${index}]`,
        );
        if (unknown) return unknown;
        const key = boundedString(item.key, `delta.remove[${index}].key`, LIVE_V2_LIMITS.keyLength);
        if (typeof key !== 'string') return key;
        if (item.cause !== 'delete' && item.cause !== 'project') {
            return wireError('invalid-field', `delta.remove[${index}].cause is unknown`);
        }
        if (seen.has(key)) return wireError('duplicate', `duplicate remove key ${key}`);
        seen.add(key);
        result.push({ key, cause: item.cause });
    }
    return result;
}

function decodeUpsert(value: unknown): LiveV2DeltaUpsert[] | LiveV2Error {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
        return wireError(
            Array.isArray(value) ? 'limit-exceeded' : 'invalid-field',
            'delta.upsert must be a bounded array',
        );
    }
    const seen = new Set<string>();
    const result: LiveV2DeltaUpsert[] = [];
    for (let index = 0; index < value.length; index += 1) {
        const item = value[index];
        if (!isRecord(item)) return wireError('invalid-field', `delta.upsert[${index}]`);
        const unknown = rejectUnknownFields(
            item,
            new Set(['key', 'row', 'card']),
            `delta.upsert[${index}]`,
        );
        if (unknown) return unknown;
        const key = boundedString(item.key, `delta.upsert[${index}].key`, LIVE_V2_LIMITS.keyLength);
        if (typeof key !== 'string') return key;
        const row = htmlString(
            item.row,
            `delta.upsert[${index}].row`,
            LIVE_V2_LIMITS.fragmentBytes,
        );
        if (typeof row !== 'string') return row;
        let card: string | undefined;
        if (own(item, 'card')) {
            const decoded = htmlString(
                item.card,
                `delta.upsert[${index}].card`,
                LIVE_V2_LIMITS.fragmentBytes,
            );
            if (typeof decoded !== 'string') return decoded;
            card = decoded;
        }
        if (seen.has(key)) return wireError('duplicate', `duplicate upsert key ${key}`);
        seen.add(key);
        result.push(card === undefined ? { key, row } : { key, row, card });
    }
    return result;
}

function decodeOrder(value: unknown): string[] | LiveV2Error {
    if (!Array.isArray(value) || value.length > LIVE_V2_LIMITS.operations) {
        return wireError(
            Array.isArray(value) ? 'limit-exceeded' : 'invalid-field',
            'delta.order must be a bounded array',
        );
    }
    const result: string[] = [];
    const seen = new Set<string>();
    for (let index = 0; index < value.length; index += 1) {
        const key = boundedString(value[index], `delta.order[${index}]`, LIVE_V2_LIMITS.keyLength);
        if (typeof key !== 'string') return key;
        if (seen.has(key)) return wireError('duplicate', `duplicate order key ${key}`);
        seen.add(key);
        result.push(key);
    }
    return result;
}

function decodeRegions(value: unknown): LiveV2DeltaRegion[] | LiveV2Error {
    if (!Array.isArray(value) || value.length > 3) {
        return wireError(
            Array.isArray(value) ? 'limit-exceeded' : 'invalid-field',
            'delta.regions must be a bounded array',
        );
    }
    const seen = new Set<LiveV2RegionName>();
    const result: LiveV2DeltaRegion[] = [];
    for (let index = 0; index < value.length; index += 1) {
        const item = value[index];
        if (!isRecord(item)) return wireError('invalid-field', `delta.regions[${index}]`);
        const unknown = rejectUnknownFields(
            item,
            new Set(['region', 'html']),
            `delta.regions[${index}]`,
        );
        if (unknown) return unknown;
        if (item.region !== 'count' && item.region !== 'phase' && item.region !== 'found') {
            return wireError('invalid-field', `delta.regions[${index}].region is unknown`);
        }
        const html = htmlString(
            item.html,
            `delta.regions[${index}].html`,
            LIVE_V2_LIMITS.fragmentBytes,
        );
        if (typeof html !== 'string') return html;
        if (seen.has(item.region)) {
            return wireError('duplicate', `duplicate region ${item.region}`);
        }
        seen.add(item.region);
        result.push({ region: item.region, html });
    }
    return result;
}

function decodeDelta(record: JSONRecord): DecodeLiveV2Result {
    const allowed = new Set([...BASE_FIELDS, 'delta']);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (!own(record, 'rev') || !own(record, 'schema') || !isRecord(record.delta)) {
        return {
            ok: false,
            error: wireError('invalid-field', 'delta, envelope rev, and schema are required'),
        };
    }
    const delta = record.delta;
    const nestedUnknown = rejectUnknownFields(
        delta,
        new Set(['base', 'rev', 'remove', 'upsert', 'order', 'regions']),
        'delta',
    );
    if (nestedUnknown) return { ok: false, error: nestedUnknown };
    const base = boundedString(delta.base, 'delta.base', LIVE_V2_LIMITS.revisionLength);
    if (typeof base !== 'string') return { ok: false, error: base };
    const rev = boundedString(delta.rev, 'delta.rev', LIVE_V2_LIMITS.revisionLength);
    if (typeof rev !== 'string') return { ok: false, error: rev };
    if (record.rev !== rev) {
        return {
            ok: false,
            error: wireError('invalid-field', 'envelope.rev must equal delta.rev'),
        };
    }
    if (base === rev) {
        return { ok: false, error: wireError('no-op', 'delta base and revision must differ') };
    }
    // The whole delta frame is already capped at deltaBytes in UTF-8. Its JSON
    // syntax and string encoding strictly dominate both aggregate decoded HTML
    // bytes and any >operations collection of valid remove/upsert objects.
    // Per-array and per-fragment caps remain explicit for local diagnostics.
    const result: LiveV2Delta = { base, rev };
    if (own(delta, 'remove')) {
        const remove = decodeRemove(delta.remove);
        if (!Array.isArray(remove)) return { ok: false, error: remove };
        result.remove = remove;
    }
    if (own(delta, 'upsert')) {
        const upsert = decodeUpsert(delta.upsert);
        if (!Array.isArray(upsert)) return { ok: false, error: upsert };
        result.upsert = upsert;
    }
    const removeKeys = new Set(result.remove?.map((item) => item.key));
    const overlap = result.upsert?.find((item) => removeKeys.has(item.key));
    if (overlap) {
        return {
            ok: false,
            error: wireError('duplicate', `key ${overlap.key} is removed and upserted`),
        };
    }
    if (own(delta, 'order')) {
        const order = decodeOrder(delta.order);
        if (!Array.isArray(order)) return { ok: false, error: order };
        result.order = order;
    }
    if (own(delta, 'regions')) {
        const regions = decodeRegions(delta.regions);
        if (!Array.isArray(regions)) return { ok: false, error: regions };
        result.regions = regions;
    }
    if (
        (result.remove?.length || 0) === 0 &&
        (result.upsert?.length || 0) === 0 &&
        (result.order?.length || 0) === 0 &&
        (result.regions?.length || 0) === 0
    ) {
        return { ok: false, error: wireError('no-op', 'delta has no semantic operations') };
    }
    return {
        ok: true,
        value: sealDecodedEnvelope({
            ...(record as unknown as LiveV2DeltaEnvelope),
            delta: result,
        }),
    };
}

function decodeTerminal(record: JSONRecord): DecodeLiveV2Result {
    const allowed = new Set([...BASE_FIELDS, 'reason']);
    const unknown = rejectUnknownFields(record, allowed, ROOT_PATH);
    if (unknown) return { ok: false, error: unknown };
    if (
        record.reason !== 'idle' &&
        record.reason !== 'auth' &&
        record.reason !== 'watch-failed' &&
        record.reason !== 'shutdown'
    ) {
        return { ok: false, error: wireError('invalid-field', 'terminal reason is unknown') };
    }
    return {
        ok: true,
        value: sealDecodedEnvelope(record as unknown as LiveV2TerminalEnvelope),
    };
}

// Total by construction: arbitrary bytes represented as a JS string either
// produce a typed envelope or a bounded diagnostic; no exception escapes.
export function decodeLiveV2Envelope(frame: string): DecodeLiveV2Result {
    try {
        if (typeof frame !== 'string' || frame.length > LIVE_V2_LIMITS.frameBytes) {
            return { ok: false, error: wireError('limit-exceeded', 'Live v2 frame is too large') };
        }
        const parsed: unknown = JSON.parse(frame);
        if (hasDuplicateJSONMembers(frame)) {
            return {
                ok: false,
                error: wireError('duplicate', 'JSON object member names must be unique'),
            };
        }
        if (!isRecord(parsed)) {
            return { ok: false, error: wireError('invalid-frame', 'frame root must be an object') };
        }
        const maxFrameBytes =
            parsed.kind === 'delta' ? LIVE_V2_LIMITS.deltaBytes : LIVE_V2_LIMITS.frameBytes;
        // The code-unit precheck precedes TextEncoder so a delta that is already
        // too large cannot amplify again while we compute its UTF-8 size.
        if (frame.length > maxFrameBytes) {
            return { ok: false, error: wireError('limit-exceeded', 'Live v2 frame is too large') };
        }
        const frameByteLength = textEncoder.encode(frame).byteLength;
        if (frameByteLength > maxFrameBytes) {
            return { ok: false, error: wireError('limit-exceeded', 'Live v2 frame is too large') };
        }
        const baseError = decodeBase(parsed);
        if (baseError) return { ok: false, error: baseError };
        if (parsed.kind === 'snapshot') return decodeSnapshot(parsed);
        if (parsed.kind === 'delta') return decodeDelta(parsed);
        if (parsed.kind === 'terminal') return decodeTerminal(parsed);
        return { ok: false, error: wireError('unknown-kind', 'kind is unknown') };
    } catch {
        return { ok: false, error: wireError('invalid-frame', 'frame is not valid JSON') };
    }
}

function validateCursor(envelope: LiveV2DeltaEnvelope, cursor: LiveV2Cursor): LiveV2Error | null {
    if (envelope.g !== cursor.g) {
        return wireError('generation-mismatch', 'delta generation does not match the cursor');
    }
    if (envelope.screen !== cursor.screen) {
        return wireError('screen-mismatch', 'delta screen does not match the cursor');
    }
    if (envelope.seq !== cursor.seq + 1) {
        return wireError('sequence-gap', 'delta sequence is not the cursor successor');
    }
    if (envelope.delta.base !== cursor.rev) {
        return wireError('base-mismatch', 'delta base does not match the cursor revision');
    }
    if (envelope.schema !== cursor.schema) {
        return wireError('schema-mismatch', 'delta schema does not match the cursor schema');
    }
    return null;
}

function decodeApplyInput(input: unknown): DecodeLiveV2Result {
    if (typeof input === 'object' && input !== null && decodedEnvelopeTokens.has(input)) {
        return { ok: true, value: input as LiveV2Envelope };
    }
    if (typeof input === 'string') return decodeLiveV2Envelope(input);
    return {
        ok: false,
        error: wireError(
            'invalid-frame',
            'Live v2 apply input must be a raw frame or an opaque decoder result',
        ),
    };
}

// The only public mutation boundary. Validation is inseparable: a caller passes
// either raw JSON or the immutable opaque result of decodeLiveV2Envelope. It
// does not await or dispatch events. Projection revision and the returned stream
// cursor become observable only after every derived view reconciles.
export function applyLiveV2Delta(
    input: unknown,
    cursor: LiveV2Cursor,
    hooks: LiveV2ApplyTestHooks = {},
): ApplyLiveV2DeltaResult {
    const decoded = decodeApplyInput(input);
    if (!decoded.ok) return decoded;
    const envelope = decoded.value;
    if (envelope.kind !== 'delta') {
        return { ok: false, error: wireError('not-delta', 'only delta envelopes can be applied') };
    }
    const cursorError = validateCursor(envelope, cursor);
    if (cursorError) return { ok: false, error: cursorError };

    const plan: ListProjectionDeltaPlan = {
        remove: envelope.delta.remove || [],
        upsert: envelope.delta.upsert || [],
        order: envelope.delta.order,
        regions: envelope.delta.regions || [],
    };
    const deletedKeys = new Set(
        plan.remove
            .filter((operation) => operation.cause === 'delete')
            .map((operation) => operation.key),
    );
    const virtualizerCheckpoint = takeVirtualizerCheckpoint();
    const filterCheckpoint = takeLiveNameFilterCheckpoint();
    // Selection is only mutated by a true delete. Keep the common existing-row
    // upsert O(ops) before its intentional filter/window reconciliation instead
    // of cloning the potentially large cross-filter selection map every tick.
    const rowStateCheckpoint = deletedKeys.size > 0 ? takeRowStateCheckpoint() : null;
    const result = applyListProjectionDeltaTransaction(plan, {
        morph: hooks.morph,
        reconcile: () => {
            hooks.beforeReconcile?.();
            applyLiveRowDeletions(deletedKeys);
            applyLiveNameFilter();
            if (!virtualizerActive()) reapplyRowState();
            hooks.afterReconcile?.();
        },
        restoreExternalState: () => {
            // A broken restore seam must not prevent the other independent
            // stores from getting their best-effort rollback attempt.
            let failed = false;
            try {
                restoreVirtualizerCheckpoint(virtualizerCheckpoint);
            } catch {
                failed = true;
            }
            try {
                restoreLiveNameFilterCheckpoint(filterCheckpoint);
            } catch {
                failed = true;
            }
            if (rowStateCheckpoint) {
                try {
                    restoreRowStateCheckpoint(rowStateCheckpoint);
                } catch {
                    failed = true;
                }
            }
            if (failed) throw new Error();
        },
    });
    if (!result.ok) return result;

    const nextCursor: LiveV2Cursor = {
        g: envelope.g,
        seq: envelope.seq,
        screen: envelope.screen,
        rev: envelope.rev,
        schema: envelope.schema,
    };
    if (envelope.rv !== undefined) nextCursor.rv = envelope.rv;
    else if (cursor.rv !== undefined) nextCursor.rv = cursor.rv;
    return {
        ok: true,
        cursor: nextCursor,
        summary: result.summary,
    };
}
