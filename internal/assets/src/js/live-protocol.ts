// Live v2 validates only the wire envelope/cursor. Server templates own markup;
// list-projection checks just the root identity needed to mount each fragment.

import {
    applyListProjectionDelta,
    type ListProjectionDeltaPlan,
    type ListProjectionDeltaSummary,
} from './list-projection.js';

export const LIST_DELTA_APPLIED_EVENT = 'ro:list-delta-applied';

export type LiveV2RegionName = 'count' | 'phase' | 'found';

interface LiveV2EnvelopeBase {
    v: 2;
    g: string;
    seq: number;
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
    cause: 'delete' | 'project';
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

// The server's own terminal vocabulary (internal/web/handlers_stream.go): the
// session either lost its authorization, hit the pod's drain, exceeded the hard
// 12h lifetime, its shared watch failed, or the server could not put this list
// on the wire at all. `auth` and `protocol` are the two the client cannot retry
// through -- replaying the request reproduces them.
export interface LiveV2TerminalEnvelope extends LiveV2EnvelopeBase {
    kind: 'terminal';
    reason: 'auth' | 'lifetime' | 'protocol' | 'shutdown' | 'watch-failed';
}

export type LiveV2Envelope = LiveV2SnapshotEnvelope | LiveV2DeltaEnvelope | LiveV2TerminalEnvelope;

export interface LiveV2Cursor {
    g: string;
    seq: number;
    rev: string;
    rv?: string;
    schema: string;
}

export type DecodeLiveV2Result = { ok: true; value: LiveV2Envelope } | { ok: false };

export type ApplyLiveV2DeltaResult =
    | { ok: true; cursor: LiveV2Cursor; summary: ListProjectionDeltaSummary }
    | { ok: false };

export interface ListDeltaAppliedDetail {
    readonly deletedKeys: ReadonlySet<string>;
    readonly focusKey: string | null;
    readonly kind: 'delta';
    readonly previousByKey: ReadonlyMap<string, HTMLElement>;
    readonly summary: ListProjectionDeltaSummary;
}

type JSONRecord = Record<string, unknown>;

const BASE_FIELDS = new Set(['v', 'kind', 'g', 'seq', 'rev', 'rv', 'schema']);
const decodedEnvelopes = new WeakSet<object>();

function own(record: JSONRecord, key: string): boolean {
    return Object.hasOwn(record, key);
}

function exactFields(record: JSONRecord, allowed: ReadonlySet<string>): boolean {
    return Object.keys(record).every((key) => allowed.has(key));
}

function nonemptyString(value: unknown): value is string {
    return typeof value === 'string' && value.length > 0;
}

function seal<T extends LiveV2Envelope>(value: T): T {
    decodedEnvelopes.add(value);
    return value;
}

function decodeBase(record: JSONRecord): boolean {
    return (
        record.v === 2 &&
        nonemptyString(record.g) &&
        Number.isSafeInteger(record.seq) &&
        (record.seq as number) > 0 &&
        (!own(record, 'rev') || nonemptyString(record.rev)) &&
        (!own(record, 'rv') || nonemptyString(record.rv)) &&
        (!own(record, 'schema') || nonemptyString(record.schema))
    );
}

function decodeSnapshot(record: JSONRecord): DecodeLiveV2Result {
    const snapshot = record.snapshot as JSONRecord;
    if (
        !exactFields(record, new Set([...BASE_FIELDS, 'snapshot'])) ||
        !nonemptyString(record.rev) ||
        !nonemptyString(record.schema) ||
        !exactFields(snapshot, new Set(['html'])) ||
        !nonemptyString(snapshot.html)
    ) {
        return { ok: false };
    }
    return { ok: true, value: seal(record as unknown as LiveV2SnapshotEnvelope) };
}

function decodeRemove(value: unknown): LiveV2DeltaRemove[] | null {
    if (!Array.isArray(value)) return null;
    const seen = new Set<string>();
    const result: LiveV2DeltaRemove[] = [];
    for (const valueItem of value) {
        const item = valueItem as JSONRecord;
        if (
            !exactFields(item, new Set(['key', 'cause'])) ||
            !nonemptyString(item.key) ||
            (item.cause !== 'delete' && item.cause !== 'project') ||
            seen.has(item.key)
        ) {
            return null;
        }
        seen.add(item.key);
        result.push({ key: item.key, cause: item.cause });
    }
    return result;
}

function decodeUpsert(value: unknown): LiveV2DeltaUpsert[] | null {
    if (!Array.isArray(value)) return null;
    const seen = new Set<string>();
    const result: LiveV2DeltaUpsert[] = [];
    for (const valueItem of value) {
        const item = valueItem as JSONRecord;
        if (
            !exactFields(item, new Set(['key', 'row', 'card'])) ||
            !nonemptyString(item.key) ||
            !nonemptyString(item.row) ||
            (own(item, 'card') && !nonemptyString(item.card)) ||
            seen.has(item.key)
        ) {
            return null;
        }
        seen.add(item.key);
        result.push(
            own(item, 'card')
                ? { key: item.key, row: item.row, card: item.card as string }
                : { key: item.key, row: item.row },
        );
    }
    return result;
}

function decodeOrder(value: unknown): string[] | null {
    if (!Array.isArray(value)) return null;
    const seen = new Set<string>();
    const result: string[] = [];
    for (const key of value) {
        if (!nonemptyString(key) || seen.has(key)) return null;
        seen.add(key);
        result.push(key);
    }
    return result;
}

function decodeRegions(value: unknown): LiveV2DeltaRegion[] | null {
    if (!Array.isArray(value)) return null;
    const seen = new Set<LiveV2RegionName>();
    const result: LiveV2DeltaRegion[] = [];
    for (const valueItem of value) {
        const item = valueItem as JSONRecord;
        if (
            !exactFields(item, new Set(['region', 'html'])) ||
            (item.region !== 'count' && item.region !== 'phase' && item.region !== 'found') ||
            !nonemptyString(item.html) ||
            seen.has(item.region)
        ) {
            return null;
        }
        seen.add(item.region);
        result.push({ region: item.region, html: item.html });
    }
    return result;
}

function decodeDelta(record: JSONRecord): DecodeLiveV2Result {
    const wireDelta = record.delta as JSONRecord;
    if (
        !exactFields(record, new Set([...BASE_FIELDS, 'delta'])) ||
        !nonemptyString(record.rev) ||
        !nonemptyString(record.schema) ||
        !exactFields(wireDelta, new Set(['base', 'rev', 'remove', 'upsert', 'order', 'regions'])) ||
        !nonemptyString(wireDelta.base) ||
        !nonemptyString(wireDelta.rev) ||
        wireDelta.rev !== record.rev
    ) {
        return { ok: false };
    }

    const delta: LiveV2Delta = { base: wireDelta.base, rev: wireDelta.rev };
    if (own(wireDelta, 'remove')) {
        const remove = decodeRemove(wireDelta.remove);
        if (!remove) return { ok: false };
        delta.remove = remove;
    }
    if (own(wireDelta, 'upsert')) {
        const upsert = decodeUpsert(wireDelta.upsert);
        if (!upsert) return { ok: false };
        delta.upsert = upsert;
    }
    const removed = new Set(delta.remove?.map((operation) => operation.key));
    if (delta.upsert?.some((operation) => removed.has(operation.key))) {
        return { ok: false };
    }
    if (own(wireDelta, 'order')) {
        const order = decodeOrder(wireDelta.order);
        if (!order) return { ok: false };
        delta.order = order;
    }
    if (own(wireDelta, 'regions')) {
        const regions = decodeRegions(wireDelta.regions);
        if (!regions) return { ok: false };
        delta.regions = regions;
    }
    return {
        ok: true,
        value: seal({ ...(record as unknown as LiveV2DeltaEnvelope), delta }),
    };
}

function decodeTerminal(record: JSONRecord): DecodeLiveV2Result {
    if (
        !exactFields(record, new Set([...BASE_FIELDS, 'reason'])) ||
        (record.reason !== 'auth' &&
            record.reason !== 'lifetime' &&
            record.reason !== 'protocol' &&
            record.reason !== 'shutdown' &&
            record.reason !== 'watch-failed')
    ) {
        return { ok: false };
    }
    return { ok: true, value: seal(record as unknown as LiveV2TerminalEnvelope) };
}

export function decodeLiveV2Envelope(frame: string): DecodeLiveV2Result {
    try {
        const parsed = JSON.parse(frame) as JSONRecord;
        if (!decodeBase(parsed)) {
            return { ok: false };
        }
        if (parsed.kind === 'snapshot') return decodeSnapshot(parsed);
        if (parsed.kind === 'delta') return decodeDelta(parsed);
        if (parsed.kind === 'terminal') return decodeTerminal(parsed);
        return { ok: false };
    } catch {
        return { ok: false };
    }
}

function validateCursor(envelope: LiveV2DeltaEnvelope, cursor: LiveV2Cursor): boolean {
    return (
        envelope.g === cursor.g &&
        envelope.seq === cursor.seq + 1 &&
        envelope.delta.base === cursor.rev &&
        envelope.schema === cursor.schema
    );
}

function decodeApplyInput(input: unknown): DecodeLiveV2Result {
    if (typeof input === 'string') return decodeLiveV2Envelope(input);
    if (typeof input === 'object' && input !== null && decodedEnvelopes.has(input)) {
        return { ok: true, value: input as LiveV2Envelope };
    }
    return { ok: false };
}

export function applyLiveV2Delta(input: unknown, cursor: LiveV2Cursor): ApplyLiveV2DeltaResult {
    const decoded = decodeApplyInput(input);
    if (!decoded.ok || decoded.value.kind !== 'delta' || !validateCursor(decoded.value, cursor)) {
        return { ok: false };
    }
    const envelope = decoded.value;
    const plan: ListProjectionDeltaPlan = {
        remove: envelope.delta.remove || [],
        upsert: envelope.delta.upsert || [],
        order: envelope.delta.order,
        regions: envelope.delta.regions || [],
    };
    const applied = applyListProjectionDelta(plan);
    if (!applied.ok) return applied;

    const detail: ListDeltaAppliedDetail = {
        kind: 'delta',
        deletedKeys: new Set(
            plan.remove
                .filter((operation) => operation.cause === 'delete')
                .map((operation) => operation.key),
        ),
        focusKey: applied.focusKey,
        previousByKey: applied.previousByKey,
        summary: applied.summary,
    };
    document.dispatchEvent(
        new CustomEvent<ListDeltaAppliedDetail>(LIST_DELTA_APPLIED_EVENT, { detail }),
    );
    applied.restoreFocus();

    const nextCursor: LiveV2Cursor = {
        g: envelope.g,
        seq: envelope.seq,
        rev: envelope.rev,
        schema: envelope.schema,
    };
    if (envelope.rv !== undefined) nextCursor.rv = envelope.rv;
    else if (cursor.rv !== undefined) nextCursor.rv = cursor.rv;
    return { ok: true, cursor: nextCursor, summary: applied.summary };
}
