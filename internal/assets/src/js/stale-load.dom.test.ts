// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

const refresh = vi.hoisted(() => ({
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
        <aside class="ro-stale-banner" hidden>
            <p class="bn-title">Ordinary stale state</p>
            <p class="bn-text">Retrying in <span data-stale-countdown></span>.</p>
            <button data-ro-action="retry">Retry now</button>
        </aside>`;
    const content = document.getElementById('resource-list-content') as HTMLElement;
    const banner = document.querySelector('.ro-stale-banner');

    stale.markLiveUnavailable();
    expect(content).not.toHaveClass('ro-stale');
    stale.clearLiveUnavailable();
    expect(banner).not.toBeVisible();

    stale.markListStale();
    expect(content).toHaveClass('ro-stale');
    expect(banner).not.toHaveAttribute('aria-label');
    stale.clearListStale();
    expect(banner).not.toBeVisible();

    for (const eventType of ['htmx:responseError', 'htmx:sendError']) {
        document.dispatchEvent(new CustomEvent(eventType, { detail: { target: content } }));

        expect(refresh.noteRefreshFailure).toHaveBeenCalledOnce();
        expect(content.className).toBe('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).toBeVisible();

        stale.clearListStale();
        refresh.noteRefreshFailure.mockClear();
    }
});
