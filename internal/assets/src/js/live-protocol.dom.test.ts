// @vitest-environment jsdom

import { beforeEach, expect, test, vi } from 'vitest';
import {
    adoptListProjection,
    listProjectionOrder,
    listProjectionRevision,
    listProjectionRowByKey,
    listProjectionRowModel,
    resetListProjection,
} from './list-projection.js';
import {
    applyLiveV2Delta,
    decodeLiveV2Envelope,
    LIST_DELTA_APPLIED_EVENT,
    type ListDeltaAppliedDetail,
    type LiveV2Cursor,
    type LiveV2Delta,
    type LiveV2DeltaEnvelope,
} from './live-protocol.js';

function row(key: string, name: string): string {
    return `<tr id="row-${key}" data-key="${key}" data-name="${name}"><td class="cell-name"><a href="#${key}">${name}</a></td><td>Ready</td></tr>`;
}

function mount(keys: readonly string[]): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.innerHTML = `
        <div class="ro-table-wrap">
            <table class="ro-table">
                <thead><tr><th>Name</th><th>Status</th></tr></thead>
                <tbody>${keys.map((key) => row(key, `Name ${key}`)).join('')}</tbody>
            </table>
        </div>
        <span data-ro-live-region="count">${keys.length}</span>
        <div id="ro-live-status"></div>`;
    document.body.append(content);
    adoptListProjection(content);
    return content;
}

function cursor(overrides: Partial<LiveV2Cursor> = {}): LiveV2Cursor {
    return {
        g: 'generation-1',
        seq: 1,
        rev: 'rev-1',
        rv: '10',
        schema: 'schema-1',
        ...overrides,
    };
}

function frame(
    body: Partial<LiveV2Delta>,
    overrides: Partial<Omit<LiveV2DeltaEnvelope, 'delta'>> = {},
): string {
    return JSON.stringify({
        v: 2,
        kind: 'delta',
        g: 'generation-1',
        seq: 2,
        rev: 'rev-2',
        rv: '11',
        schema: 'schema-1',
        ...overrides,
        delta: { base: 'rev-1', rev: 'rev-2', ...body },
    });
}

function morphInPlace(current: HTMLElement, incoming: HTMLElement): HTMLElement[] {
    for (const attribute of Array.from(current.attributes)) current.removeAttribute(attribute.name);
    for (const attribute of Array.from(incoming.attributes)) {
        current.setAttribute(attribute.name, attribute.value);
    }
    current.replaceChildren(...Array.from(incoming.childNodes, (child) => child.cloneNode(true)));
    return [current];
}

beforeEach(() => {
    document.body.replaceChildren();
    resetListProjection();
    vi.stubGlobal('Idiomorph', { morph: vi.fn(morphInPlace) });
});

test('commits DOM/model, runs the post-update signal synchronously, then publishes the cursor', () => {
    mount(['dev/a', 'dev/b']);
    const observed: Array<{ detail: ListDeltaAppliedDetail; order: readonly string[] }> = [];
    document.addEventListener(
        LIST_DELTA_APPLIED_EVENT,
        (event) => {
            observed.push({
                detail: (event as CustomEvent<ListDeltaAppliedDetail>).detail,
                order: [...listProjectionOrder()],
            });
        },
        { once: true },
    );

    const decoded = decodeLiveV2Envelope(
        frame({
            remove: [{ key: 'dev/a', cause: 'delete' }],
            upsert: [
                { key: 'dev/b', row: row('dev/b', 'Beta updated') },
                { key: 'dev/c', row: row('dev/c', 'Gamma') },
            ],
            order: ['dev/b', 'dev/c'],
            regions: [
                {
                    region: 'count',
                    html: '<strong data-ro-live-region="count">2 current</strong>',
                },
            ],
        }),
    );
    if (!decoded.ok) throw new Error('fixture must decode');

    const result = applyLiveV2Delta(decoded.value, cursor());

    expect(result).toMatchObject({
        ok: true,
        cursor: {
            g: 'generation-1',
            seq: 2,
            rev: 'rev-2',
            rv: '11',
            schema: 'schema-1',
        },
        summary: { inserted: 1, updated: 1, deleted: 1, reordered: true },
    });
    expect(observed).toHaveLength(1);
    expect(observed[0]?.order).toStrictEqual(['dev/b', 'dev/c']);
    expect(observed[0]?.detail.deletedKeys).toStrictEqual(new Set(['dev/a']));
    expect(observed[0]?.detail.previousByKey.get('dev/b')).toHaveTextContent('Name dev/b');
    expect(listProjectionRowByKey('dev/b')).toHaveTextContent('Beta updated');
    expect(listProjectionRowByKey('dev/a')).toBeNull();
    expect(listProjectionRowByKey('dev/c')).toHaveTextContent('Gamma');
    expect(listProjectionRowModel().rows.map((model) => model.key)).toStrictEqual([
        'dev/b',
        'dev/c',
    ]);
    expect(document.querySelector('[data-ro-live-region="count"]')).toHaveTextContent('2 current');
});

test('projected rows leave the true-deletion set empty', () => {
    mount(['dev/a', 'dev/b']);
    let detail: ListDeltaAppliedDetail | null = null;
    document.addEventListener(
        LIST_DELTA_APPLIED_EVENT,
        (event) => {
            detail = (event as CustomEvent<ListDeltaAppliedDetail>).detail;
        },
        { once: true },
    );

    const result = applyLiveV2Delta(
        frame({
            remove: [{ key: 'dev/a', cause: 'project' }],
            order: ['dev/b'],
        }),
        cursor(),
    );

    expect(result.ok).toBe(true);
    expect(detail).toMatchObject({ kind: 'delta', deletedKeys: new Set() });
});

const cursorBreaks: Array<
    [string, Partial<Omit<LiveV2DeltaEnvelope, 'delta'>>, Partial<LiveV2Delta>?]
> = [
    ['generation', { g: 'other' }],
    ['sequence', { seq: 4 }],
    ['base revision', {}, { base: 'other' }],
    ['schema', { schema: 'other' }],
] as const;

test.each(cursorBreaks)(
    'rejects a %s cursor break without touching DOM/model',
    (_name, envelope, body: Partial<LiveV2Delta> = {}) => {
        const content = mount(['dev/a']);
        const before = content.innerHTML;
        const revision = listProjectionRevision();

        const result = applyLiveV2Delta(
            frame({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Changed') }], ...body }, envelope),
            cursor(),
        );

        expect(result).toStrictEqual({ ok: false });
        expect(content.innerHTML).toBe(before);
        expect(listProjectionRevision()).toBe(revision);
    },
);

test('rejects a malformed fragment root without publishing a new cursor', () => {
    mount(['dev/a']);

    const result = applyLiveV2Delta(
        frame({ upsert: [{ key: 'dev/a', row: row('dev/other', 'Wrong') }] }),
        cursor(),
    );

    expect(result).toStrictEqual({ ok: false });
    expect(listProjectionRowByKey('dev/a')).toHaveTextContent('Name dev/a');
});

test('rejects an explicit order that omits a surviving row', () => {
    const content = mount(['dev/a', 'dev/b']);
    const before = content.innerHTML;

    const result = applyLiveV2Delta(frame({ order: ['dev/a'] }), cursor());

    expect(result).toStrictEqual({ ok: false });
    expect(content.innerHTML).toBe(before);
    expect(listProjectionOrder()).toStrictEqual(['dev/a', 'dev/b']);
});

test('preserves the prior resource version when the delta omits one', () => {
    mount(['dev/a']);

    const result = applyLiveV2Delta(
        frame({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Changed') }] }, { rv: undefined }),
        cursor({ rv: 'previous-rv' }),
    );

    expect(result).toMatchObject({ ok: true, cursor: { rv: 'previous-rv' } });
});

test('keeps rv absent when both the delta and prior cursor omit it', () => {
    mount(['dev/a']);

    const result = applyLiveV2Delta(
        frame({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Changed') }] }, { rv: undefined }),
        cursor({ rv: undefined }),
    );

    if (!result.ok) throw new Error('fixture must apply');
    expect(Object.hasOwn(result.cursor, 'rv')).toBe(false);
});

test('restores focused cell content after the synchronous update pipeline', () => {
    mount(['dev/a']);
    const oldLink = listProjectionRowByKey('dev/a')?.querySelector('a') as HTMLAnchorElement;
    oldLink.focus();
    let observedFocusKey: string | null = null;
    document.addEventListener(
        LIST_DELTA_APPLIED_EVENT,
        (event) => {
            observedFocusKey = (event as CustomEvent<ListDeltaAppliedDetail>).detail.focusKey;
            // Model the virtualizer detaching and reattaching the canonical row
            // during the shared synchronous post-update pipeline.
            const tbody = document.querySelector('tbody') as HTMLTableSectionElement;
            const current = listProjectionRowByKey('dev/a') as HTMLElement;
            tbody.replaceChildren(current);
        },
        { once: true },
    );

    const result = applyLiveV2Delta(
        frame({ upsert: [{ key: 'dev/a', row: row('dev/a', 'Replacement') }] }),
        cursor(),
    );

    expect(result.ok).toBe(true);
    expect(observedFocusKey).toBe('dev/a');
    expect(document.activeElement).toBe(listProjectionRowByKey('dev/a')?.querySelector('a'));
    expect(document.activeElement).toHaveTextContent('Replacement');
});
