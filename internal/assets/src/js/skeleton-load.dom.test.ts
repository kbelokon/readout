// @vitest-environment jsdom

import { afterEach, expect, test, vi } from 'vitest';

const stale = vi.hoisted(() => ({
    isListRefreshEvent: vi.fn((event: Event) => Boolean((event as CustomEvent).detail?.isList)),
}));

vi.mock('./stale.js', () => stale);

afterEach(() => {
    document.body.replaceChildren();
    vi.restoreAllMocks();
});

test('the complete skeleton lifecycle works through registered events on a fresh import', async () => {
    vi.resetModules();
    await import('./skeleton.js');

    document.body.innerHTML = `
        <div id="ro-skel-template" hidden>
            <div class="ro-skel">Loading A</div>
            <div class="ro-skel">Loading B</div>
        </div>
        <div id="resource-list-content"></div>`;
    const content = document.getElementById('resource-list-content') as HTMLElement;
    const requestEvent = (type: string) => new CustomEvent(type, { detail: { isList: true } });

    document.dispatchEvent(requestEvent('htmx:beforeRequest'));
    expect(content.querySelectorAll(':scope > .ro-skel')).toHaveLength(2);

    document.dispatchEvent(requestEvent('htmx:responseError'));
    expect(content.querySelectorAll(':scope > .ro-skel')).toHaveLength(0);

    document.dispatchEvent(requestEvent('htmx:beforeRequest'));
    document.dispatchEvent(requestEvent('htmx:sendError'));
    expect(content.querySelectorAll(':scope > .ro-skel')).toHaveLength(0);
});
