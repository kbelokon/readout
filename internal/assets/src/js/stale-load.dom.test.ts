// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

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
            <div class="bn-body">
                <p class="bn-title">Ordinary stale state</p>
                <p class="bn-text">Retrying in <span data-stale-countdown></span>.</p>
            </div>
            <div class="bn-body ro-stale-unavailable" hidden><p class="bn-title">Unavailable</p></div>
            <button class="ro-stale-retry" data-ro-action="retry">Retry now</button>
            <button class="ro-stale-reload" data-ro-action="reload" hidden>Reload</button>
        </aside>`;
    const content = document.getElementById('resource-list-content') as HTMLElement;
    const banner = document.querySelector('.ro-stale-banner');

    // A Live disconnect is semantic immediately and invisible until the grace
    // expires; a fresh module has no timer armed until it is asked for one.
    stale.markLiveStale();
    expect(content.dataset.roStale).toBe('true');
    expect(content).not.toHaveClass('ro-stale');
    stale.clearLiveStale();
    expect(banner).not.toBeVisible();
    expect(content.dataset.roStale).toBeUndefined();

    stale.markLiveUnavailable();
    expect(banner).toBeVisible();
    expect(document.querySelector('.ro-stale-reload')).toBeVisible();
    stale.clearLiveStale();
    expect(banner).not.toBeVisible();

    stale.markListStale();
    expect(content).toHaveClass('ro-stale');
    stale.clearListStale();
    expect(banner).not.toBeVisible();

    for (const eventType of ['htmx:responseError', 'htmx:sendError']) {
        document.dispatchEvent(new CustomEvent(eventType, { detail: { target: content } }));

        expect(content.className).toBe('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).toBeVisible();

        stale.clearListStale();
    }
});
