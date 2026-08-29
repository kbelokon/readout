// @vitest-environment jsdom

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, vi } from 'vitest';
import {
    adoptListProjection,
    listProjectionCardByKey,
    listProjectionOrder,
    listProjectionRowByKey,
    resetListProjection,
} from './list-projection.js';
import { applyLiveV2Delta } from './live-protocol.js';

interface RenderContract {
    version: 1;
    rows: Array<{ name: string; key: string; row: string; card: string }>;
    regions: Array<{ region: 'count' | 'phase' | 'found'; html: string }>;
}

function morph(current: HTMLElement, incoming: HTMLElement): HTMLElement[] {
    current.replaceWith(incoming);
    return [incoming];
}

test('applies the canonical server-rendered Live row, card and region fixtures', () => {
    const contract = JSON.parse(
        readFileSync(
            join(process.cwd(), 'internal/web/testdata/live_render_contract.json'),
            'utf8',
        ),
    ) as RenderContract;
    expect(contract.version).toBe(1);
    expect(contract.rows.length).toBeGreaterThan(0);
    expect(contract.regions.map(({ region }) => region).sort()).toStrictEqual([
        'count',
        'found',
        'phase',
    ]);

    document.body.innerHTML = `<div id="resource-list-content">
        <div class="ro-table-wrap"><table class="ro-table"><thead><tr><th>Name</th></tr></thead><tbody></tbody></table></div>
        <div class="ro-cardlist"></div>
        <span data-ro-live-region="count"></span>
        <span data-ro-live-region="phase"></span>
        <span data-ro-live-region="found"></span>
        <div id="ro-live-status"></div>
    </div>`;
    const content = document.getElementById('resource-list-content') as HTMLElement;
    resetListProjection();
    adoptListProjection(content);
    vi.stubGlobal('Idiomorph', { morph: vi.fn(morph) });

    const keys = contract.rows.map(({ key }) => key);
    const result = applyLiveV2Delta(
        JSON.stringify({
            v: 2,
            kind: 'delta',
            g: 'contract-generation',
            seq: 2,
            rev: 'contract-rev-2',
            schema: 'contract-schema',
            delta: {
                base: 'contract-rev-1',
                rev: 'contract-rev-2',
                upsert: contract.rows.map(({ key, row, card }) => ({ key, row, card })),
                order: keys,
                regions: contract.regions,
            },
        }),
        {
            g: 'contract-generation',
            seq: 1,
            rev: 'contract-rev-1',
            schema: 'contract-schema',
        },
    );

    expect(result.ok).toBe(true);
    expect(listProjectionOrder()).toStrictEqual(keys);
    const tbody = content.querySelector('tbody') as HTMLTableSectionElement;
    const cardMount = content.querySelector('.ro-cardlist') as HTMLElement;
    expect(Array.from(tbody.children)).toStrictEqual(
        keys.map((key) => listProjectionRowByKey(key)),
    );
    expect(Array.from(cardMount.children)).toStrictEqual(
        keys.map((key) => listProjectionCardByKey(key)),
    );
    for (const { key } of contract.rows) {
        expect(listProjectionRowByKey(key)?.isConnected).toBe(true);
        expect(listProjectionCardByKey(key)?.isConnected).toBe(true);
    }
    for (const { region, html } of contract.regions) {
        const expected = document.createElement('template');
        expected.innerHTML = html;
        expect(document.querySelector(`[data-ro-live-region="${region}"]`)?.outerHTML).toBe(
            expected.content.firstElementChild?.outerHTML,
        );
    }

    const eventFixture = contract.rows.find(({ name }) => name === 'event-unknown-kind');
    expect(eventFixture).toBeDefined();
    expect(
        listProjectionRowByKey(eventFixture?.key || '')?.querySelector('[style*="--kh:"]'),
    ).not.toBeNull();
});
