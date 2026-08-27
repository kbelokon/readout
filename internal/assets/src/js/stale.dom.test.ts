// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

const refresh = vi.hoisted(() => ({
    isPreloadRequest: vi.fn((event: Event) => {
        const headers = (event as CustomEvent).detail?.requestConfig?.headers;
        return headers?.['HX-Preloaded'] === 'true';
    }),
    noteRefreshFailure: vi.fn(),
    refreshNextAtMs: vi.fn(() => 0),
}));

vi.mock('./refresh.js', () => refresh);

import {
    clearListStale,
    isListRefreshEvent,
    markListStale,
    updateStaleCountdown,
} from './stale.js';

function renderStaleUI(): void {
    document.body.innerHTML = `
        <div id="resource-list-content"><table><tbody><tr><td>last good row</td></tr></tbody></table></div>
        <aside class="ro-stale-banner" hidden>Retrying in <span data-stale-countdown>…</span></aside>
    `;
}

function htmxEvent(
    type: string,
    detail: Record<string, unknown>,
    preload = false,
): CustomEvent<Record<string, unknown>> {
    return new CustomEvent(type, {
        bubbles: true,
        detail: {
            ...detail,
            requestConfig: preload ? { headers: { 'HX-Preloaded': 'true' } } : undefined,
        },
    });
}

beforeEach(() => {
    clearListStale();
    renderStaleUI();
    refresh.refreshNextAtMs.mockReturnValue(0);
});

describe('refresh-event classification', () => {
    test('accepts requests issued by or targeting the list container', () => {
        const content = document.getElementById('resource-list-content');
        const userControl = document.createElement('button');

        expect(isListRefreshEvent(htmxEvent('x', { elt: content }))).toBe(true);
        expect(isListRefreshEvent(htmxEvent('x', { elt: userControl, target: content }))).toBe(
            true,
        );
    });

    test('rejects missing details, unrelated targets, and preload warmups', () => {
        const unrelated = document.createElement('main');
        const content = document.getElementById('resource-list-content');

        expect(isListRefreshEvent(new Event('x'))).toBe(false);
        expect(isListRefreshEvent(htmxEvent('x', { elt: unrelated, target: unrelated }))).toBe(
            false,
        );
        expect(isListRefreshEvent(htmxEvent('x', { elt: content }, true))).toBe(false);
    });
});

describe('stale UI lifecycle', () => {
    test('dims without deleting data and starts only one countdown timer', () => {
        vi.useFakeTimers();

        markListStale();
        markListStale();

        expect(document.getElementById('resource-list-content')).toHaveClass('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).toBeVisible();
        expect(document.body).toHaveTextContent('last good row');
        expect(vi.getTimerCount()).toBe(1);
    });

    test('paints a deterministic retry countdown and clamps at zero', () => {
        vi.useFakeTimers();
        vi.setSystemTime(new Date('2026-08-25T00:00:00Z'));
        refresh.refreshNextAtMs.mockReturnValue(Date.now() + 2501);

        updateStaleCountdown();
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('3s');

        refresh.refreshNextAtMs.mockReturnValue(Date.now() - 1);
        updateStaleCountdown();
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('0s');

        refresh.refreshNextAtMs.mockReturnValue(0);
        updateStaleCountdown();
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('…');
    });

    test('missing stale UI is safe and does not start a useless countdown ticker', () => {
        vi.useFakeTimers();
        document.body.replaceChildren();

        expect(() => markListStale()).not.toThrow();

        expect(vi.getTimerCount()).toBe(0);
        expect(() => updateStaleCountdown()).not.toThrow();
        expect(() => clearListStale()).not.toThrow();
    });

    test('clear restores fresh state, hides the banner, and stops the ticker', () => {
        vi.useFakeTimers();
        markListStale();

        clearListStale();

        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).not.toBeVisible();
        expect(vi.getTimerCount()).toBe(0);
    });

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s records failure before revealing stale state',
        (eventType) => {
            const content = document.getElementById('resource-list-content');

            document.dispatchEvent(htmxEvent(eventType, { target: content }));

            expect(refresh.noteRefreshFailure).toHaveBeenCalledOnce();
            expect(document.getElementById('resource-list-content')).toHaveClass('ro-stale');
            expect(refresh.noteRefreshFailure.mock.invocationCallOrder[0]).toBeLessThan(
                refresh.refreshNextAtMs.mock.invocationCallOrder[0] as number,
            );
        },
    );

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s ignores unrelated error events',
        (eventType) => {
            document.dispatchEvent(
                htmxEvent(eventType, { target: document.createElement('main') }),
            );

            expect(refresh.noteRefreshFailure).not.toHaveBeenCalled();
            expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        },
    );
});
