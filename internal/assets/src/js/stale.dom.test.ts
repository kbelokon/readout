// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import {
    clearListStale,
    clearLiveUnavailable,
    isListRefreshEvent,
    markListStale,
    markLiveUnavailable,
    noteStaleRetryAt,
    updateStaleCountdown,
} from './stale.js';

function renderStaleUI(): void {
    document.body.innerHTML = `
        <div id="resource-list-content"><table><tbody><tr><td>last good row</td></tr></tbody></table></div>
        <aside class="ro-stale-banner" hidden>
            <p class="bn-title">Auto-refresh failed</p>
            <p class="bn-text">Retrying in <span data-stale-countdown>…</span>.</p>
            <button data-ro-action="retry">Retry now</button>
        </aside>
    `;
}

function htmxEvent(
    type: string,
    detail: Record<string, unknown>,
): CustomEvent<Record<string, unknown>> {
    return new CustomEvent(type, {
        bubbles: true,
        detail,
    });
}

beforeEach(() => {
    clearLiveUnavailable();
    clearListStale();
    renderStaleUI();
    noteStaleRetryAt(0);
});

afterEach(() => {
    clearLiveUnavailable();
    clearListStale();
    vi.useRealTimers();
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

    test('rejects missing details and unrelated targets', () => {
        const unrelated = document.createElement('main');

        expect(isListRefreshEvent(new Event('x'))).toBe(false);
        expect(isListRefreshEvent(htmxEvent('x', { elt: unrelated, target: unrelated }))).toBe(
            false,
        );
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

    test('paints the published reconnect time as a countdown and clamps at zero', () => {
        vi.useFakeTimers();
        vi.setSystemTime(new Date('2026-08-25T00:00:00Z'));

        noteStaleRetryAt(Date.now() + 2501);
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('3s');

        noteStaleRetryAt(Date.now() - 1);
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('0s');

        // 0 = nothing armed: the shipped placeholder comes back.
        noteStaleRetryAt(0);
        expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('…');
    });

    test('removing the stale UI stops its active countdown ticker', () => {
        vi.useFakeTimers();

        markListStale();
        expect(vi.getTimerCount()).toBe(1);
        document.body.replaceChildren();

        expect(() => clearListStale()).not.toThrow();
        expect(vi.getTimerCount()).toBe(0);
        expect(() => updateStaleCountdown()).not.toThrow();
    });

    test('clear restores fresh state, hides the banner, and stops the ticker', () => {
        vi.useFakeTimers();
        markListStale();

        clearListStale();

        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        expect(document.querySelector('.ro-stale-banner')).not.toBeVisible();
        expect(vi.getTimerCount()).toBe(0);
    });

    test('Live-unavailable copy follows the armed reconnect schedule', () => {
        vi.useFakeTimers();
        vi.setSystemTime(new Date('2026-08-25T00:00:00Z'));
        noteStaleRetryAt(Date.now() + 5_000);

        markLiveUnavailable();

        const banner = document.querySelector('.ro-stale-banner');
        expect(banner).toBeVisible();
        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        expect(banner?.querySelector('.bn-title')).toHaveTextContent('Live unavailable, polling ·');
        expect(banner?.querySelector('[data-ro-action="retry"]')).toHaveTextContent('Retry');
        expect(banner?.querySelector('[data-stale-countdown]')).toHaveTextContent('5s');
        expect(banner).toHaveAttribute(
            'aria-label',
            'Live unavailable, polling. Retrying in 5s. Retry',
        );
        expect(vi.getTimerCount()).toBe(1);

        noteStaleRetryAt(Date.now() + 10_000);
        expect(banner?.querySelector('[data-stale-countdown]')).toHaveTextContent('10s');

        noteStaleRetryAt(Date.now() + 20_000);
        expect(banner?.querySelector('[data-stale-countdown]')).toHaveTextContent('20s');
        expect(banner).toHaveAttribute(
            'aria-label',
            'Live unavailable, polling. Retrying in 20s. Retry',
        );
    });

    test('clearing Live mode cannot hide independently stale list data', () => {
        vi.useFakeTimers();
        noteStaleRetryAt(Date.now() + 5_000);
        const banner = document.querySelector('.ro-stale-banner');

        markLiveUnavailable();

        clearListStale();
        expect(banner).toBeVisible();
        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');

        markListStale();
        clearLiveUnavailable();
        expect(banner).toBeVisible();
        expect(banner).not.toHaveAttribute('aria-label');
        expect(document.getElementById('resource-list-content')).toHaveClass('ro-stale');
        expect(banner?.querySelector('.bn-title')).toHaveTextContent('Auto-refresh failed');
        expect(banner?.querySelector('[data-ro-action="retry"]')).toHaveTextContent('Retry now');
        expect(banner?.querySelector('[data-stale-countdown]')).toBeInTheDocument();

        clearListStale();
        expect(banner).not.toBeVisible();
        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        expect(vi.getTimerCount()).toBe(0);
    });

    test('a successful polling morph cannot erase Live-unavailable ownership', () => {
        vi.useFakeTimers();
        noteStaleRetryAt(Date.now() + 5_000);
        markLiveUnavailable();

        const oldBanner = document.querySelector('.ro-stale-banner') as HTMLElement;
        oldBanner.outerHTML = `
            <aside class="ro-stale-banner" hidden>
                <p class="bn-title">Fresh server copy</p>
                <p class="bn-text">Next poll in <span data-stale-countdown>…</span>.</p>
                <button data-ro-action="retry">Try server</button>
            </aside>`;
        clearListStale();

        const banner = document.querySelector('.ro-stale-banner');
        expect(banner).toBeVisible();
        expect(banner?.querySelector('.bn-title')).toHaveTextContent('Live unavailable, polling ·');
        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');

        clearLiveUnavailable();
        expect(banner).not.toBeVisible();
        expect(banner?.querySelector('.bn-title')).toHaveTextContent('Fresh server copy');
        expect(banner?.querySelector('[data-ro-action="retry"]')).toHaveTextContent('Try server');
    });

    test('history serialization preserves the exact original nested copy and accessibility state', () => {
        vi.useFakeTimers();
        noteStaleRetryAt(Date.now() + 5_000);
        let banner = document.querySelector('.ro-stale-banner') as HTMLElement;
        let message = banner.querySelector('.bn-text') as HTMLElement;
        const originalMessage =
            'Retrying <strong>very soon</strong> in <span data-stale-countdown="">…</span>.';
        banner.setAttribute('aria-label', 'Original warning');
        (banner.querySelector('.bn-title') as HTMLElement).textContent = 'Original stale title';
        (banner.querySelector('[data-ro-action="retry"]') as HTMLElement).textContent =
            'Try original';
        message.innerHTML = originalMessage;
        message.hidden = false;

        markLiveUnavailable();
        const serializedBody = document.body.innerHTML;
        document.body.innerHTML = serializedBody;
        banner = document.querySelector('.ro-stale-banner') as HTMLElement;
        message = banner.querySelector('.bn-text') as HTMLElement;

        expect(message).toBeVisible();
        expect(banner.querySelector('.bn-title')).toHaveTextContent('Live unavailable, polling ·');
        expect(banner).toHaveAttribute(
            'aria-label',
            'Live unavailable, polling. Retrying in 5s. Retry',
        );
        clearLiveUnavailable();

        expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        expect(banner).toHaveAttribute('aria-label', 'Original warning');
        expect(banner.querySelector('.bn-title')).toHaveTextContent('Original stale title');
        expect(banner.querySelector('[data-ro-action="retry"]')).toHaveTextContent('Try original');
        expect(message.hidden).toBe(false);
        expect(message.innerHTML).toBe(originalMessage);
    });

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s reveals stale state without arming any retry',
        (eventType) => {
            vi.useFakeTimers();
            const content = document.getElementById('resource-list-content');

            document.dispatchEvent(htmxEvent(eventType, { target: content }));

            expect(document.getElementById('resource-list-content')).toHaveClass('ro-stale');
            expect(document.querySelector('.ro-stale-banner')).toBeVisible();
            // Only the 1s repaint ticker: a failed list GET schedules no retry.
            expect(vi.getTimerCount()).toBe(1);
            expect(document.querySelector('[data-stale-countdown]')?.textContent).toBe('…');
        },
    );

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s ignores unrelated error events',
        (eventType) => {
            document.dispatchEvent(
                htmxEvent(eventType, { target: document.createElement('main') }),
            );

            expect(document.getElementById('resource-list-content')).not.toHaveClass('ro-stale');
        },
    );
});
