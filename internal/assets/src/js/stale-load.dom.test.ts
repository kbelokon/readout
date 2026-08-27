// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

const refresh = vi.hoisted(() => ({
    isPreloadRequest: vi.fn(() => false),
    noteRefreshFailure: vi.fn(),
    refreshNextAtMs: vi.fn(() => 0),
}));

vi.mock('./refresh.js', () => refresh);

afterEach(() => {
    document.body.replaceChildren();
    vi.restoreAllMocks();
});

test('both HTMX error events drive the observable stale lifecycle after a fresh import', async () => {
    vi.resetModules();
    const stale = await import('./stale.js');

    document.body.innerHTML = `
        <div id="resource-list-content">last good rows</div>
        <aside class="ro-stale-banner" hidden><span data-stale-countdown></span></aside>`;
    const content = document.getElementById('resource-list-content') as HTMLElement;

    for (const eventType of ['htmx:responseError', 'htmx:sendError']) {
        document.dispatchEvent(new CustomEvent(eventType, { detail: { target: content } }));

        expect(refresh.noteRefreshFailure).toHaveBeenCalledOnce();
        expect(content.className).toBe('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).toBeVisible();

        stale.clearListStale();
        refresh.noteRefreshFailure.mockClear();
    }
});
