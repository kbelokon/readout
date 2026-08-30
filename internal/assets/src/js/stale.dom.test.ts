// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import {
    clearListStale,
    clearLiveStale,
    isListRefreshEvent,
    LIVE_STALE_GRACE_MS,
    markListStale,
    markLiveStale,
    markLiveUnavailable,
    noteStaleRetryAt,
    revealLiveStale,
    updateStaleCountdown,
} from './stale.js';

// The banner is a server template (templates/errors.templ): both copy variants
// and both actions ship hidden in the same node, and this module unhides
// exactly one of each.
function renderStaleUI(): void {
    document.body.innerHTML = `
        <div id="resource-list-content"><table><tbody><tr><td>last good row</td></tr></tbody></table></div>
        <div class="ro-banner warn ro-stale-banner" role="alert" hidden>
            <div class="bn-body">
                <p class="bn-title">Auto-refresh failed — showing the last good data</p>
                <p class="bn-text">Retrying in <span class="mono" data-stale-countdown>…</span>.</p>
            </div>
            <div class="bn-body ro-stale-unavailable" hidden>
                <p class="bn-title">Live updates unavailable — showing the last good data</p>
                <p class="bn-text">This session can no longer stream updates.</p>
            </div>
            <div class="bn-actions">
                <button type="button" class="ro-stale-retry" data-ro-action="retry">Retry now</button>
                <button type="button" class="ro-stale-reload" data-ro-action="reload" hidden>Reload</button>
            </div>
        </div>
    `;
}

function content(): HTMLElement {
    return document.getElementById('resource-list-content') as HTMLElement;
}

function banner(): HTMLElement {
    return document.querySelector('.ro-stale-banner') as HTMLElement;
}

function part(selector: string): HTMLElement {
    return banner().querySelector(selector) as HTMLElement;
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
    clearLiveStale();
    clearListStale();
    renderStaleUI();
    noteStaleRetryAt(0);
});

afterEach(() => {
    clearLiveStale();
    clearListStale();
    vi.useRealTimers();
});

describe('refresh-event classification', () => {
    test('accepts requests issued by or targeting the list container', () => {
        const userControl = document.createElement('button');

        expect(isListRefreshEvent(htmxEvent('x', { elt: content() }))).toBe(true);
        expect(isListRefreshEvent(htmxEvent('x', { elt: userControl, target: content() }))).toBe(
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

describe('list stale lifecycle', () => {
    test('dims without deleting data and starts only one countdown timer', () => {
        vi.useFakeTimers();

        markListStale();
        markListStale();

        expect(content()).toHaveClass('ro-stale');
        expect(banner()).toBeVisible();
        expect(document.body).toHaveTextContent('last good row');
        expect(vi.getTimerCount()).toBe(1);
    });

    test('a failed list request is not semantic Live staleness', () => {
        markListStale();

        expect(content().dataset.roStale).toBeUndefined();
    });

    test('paints the published reconnect time as a countdown and clamps at zero', () => {
        vi.useFakeTimers();
        vi.setSystemTime(new Date('2026-08-25T00:00:00Z'));

        noteStaleRetryAt(Date.now() + 2501);
        expect(part('[data-stale-countdown]').textContent).toBe('3s');

        noteStaleRetryAt(Date.now() - 1);
        expect(part('[data-stale-countdown]').textContent).toBe('0s');

        // 0 = nothing armed: the shipped placeholder comes back.
        noteStaleRetryAt(0);
        expect(part('[data-stale-countdown]').textContent).toBe('…');
    });

    // The banner is a closed server template, but a partial render (a template
    // edit that drops one node, an older cached document) must degrade rather
    // than take the whole stale path down with it.
    test.each([
        '.bn-body:not(.ro-stale-unavailable)',
        '.bn-body.ro-stale-unavailable',
        '.ro-stale-retry',
        '.ro-stale-reload',
        '[data-stale-countdown]',
    ])('a banner missing %s still paints both variants without throwing', (selector) => {
        vi.useFakeTimers();
        part(selector).remove();

        expect(() => markListStale()).not.toThrow();
        expect(banner()).toBeVisible();
        expect(() => markLiveUnavailable()).not.toThrow();
        expect(banner()).toBeVisible();
        expect(() => noteStaleRetryAt(Date.now() + 5000)).not.toThrow();
        expect(() => clearListStale()).not.toThrow();
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

        expect(content()).not.toHaveClass('ro-stale');
        expect(banner()).not.toBeVisible();
        expect(vi.getTimerCount()).toBe(0);
    });

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s reveals stale state without arming any retry',
        (eventType) => {
            vi.useFakeTimers();

            document.dispatchEvent(htmxEvent(eventType, { target: content() }));

            expect(content()).toHaveClass('ro-stale');
            expect(banner()).toBeVisible();
            // Only the 1s repaint ticker: a failed list GET schedules no retry.
            expect(vi.getTimerCount()).toBe(1);
            expect(part('[data-stale-countdown]').textContent).toBe('…');
        },
    );

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s ignores unrelated error events',
        (eventType) => {
            document.dispatchEvent(
                htmxEvent(eventType, { target: document.createElement('main') }),
            );

            expect(content()).not.toHaveClass('ro-stale');
        },
    );
});

describe('Live stale grace', () => {
    test('semantic staleness lands immediately and the visual waits out the grace', () => {
        vi.useFakeTimers();

        markLiveStale();

        expect(content().dataset.roStale).toBe('true');
        expect(content()).not.toHaveClass('ro-stale');
        expect(banner()).not.toBeVisible();

        vi.advanceTimersByTime(LIVE_STALE_GRACE_MS - 1);
        expect(banner()).not.toBeVisible();

        vi.advanceTimersByTime(1);
        expect(content()).toHaveClass('ro-stale');
        expect(banner()).toBeVisible();
        expect(part('.bn-body:not(.ro-stale-unavailable)')).toBeVisible();
        expect(part('.ro-stale-unavailable').hidden).toBe(true);
        expect(part('.ro-stale-retry').hidden).toBe(false);
        expect(part('.ro-stale-reload').hidden).toBe(true);
    });

    test('a reconnect inside the grace leaves no visible trace', () => {
        vi.useFakeTimers();

        markLiveStale();
        vi.advanceTimersByTime(LIVE_STALE_GRACE_MS - 1);
        clearLiveStale();
        vi.advanceTimersByTime(LIVE_STALE_GRACE_MS);

        expect(content().dataset.roStale).toBeUndefined();
        expect(content()).not.toHaveClass('ro-stale');
        expect(banner()).not.toBeVisible();
        expect(vi.getTimerCount()).toBe(0);
    });

    test('re-marking an already stale projection cannot restart the grace', () => {
        vi.useFakeTimers();

        markLiveStale();
        vi.advanceTimersByTime(LIVE_STALE_GRACE_MS - 1);
        markLiveStale();
        vi.advanceTimersByTime(1);

        expect(banner()).toBeVisible();
        markLiveStale();
        expect(banner()).toBeVisible();
    });

    test('the first failed reconnect ends the grace early', () => {
        vi.useFakeTimers();

        markLiveStale();
        revealLiveStale();

        expect(banner()).toBeVisible();
        expect(content()).toHaveClass('ro-stale');
        // The armed grace is retired, not left to fire a second paint.
        expect(vi.getTimerCount()).toBe(1);
    });

    test('the countdown follows the armed reconnect schedule', () => {
        vi.useFakeTimers();
        vi.setSystemTime(new Date('2026-08-25T00:00:00Z'));

        revealLiveStale();
        noteStaleRetryAt(Date.now() + 5_000);
        expect(part('[data-stale-countdown]')).toHaveTextContent('5s');

        noteStaleRetryAt(Date.now() + 30_000);
        expect(part('[data-stale-countdown]')).toHaveTextContent('30s');
    });
});

describe('Live unavailable', () => {
    test('swaps the copy variant and the action, with no grace and no countdown', () => {
        vi.useFakeTimers();

        markLiveUnavailable();

        expect(content().dataset.roStale).toBe('true');
        expect(content()).toHaveClass('ro-stale');
        expect(banner()).toBeVisible();
        expect(part('.bn-body:not(.ro-stale-unavailable)').hidden).toBe(true);
        expect(part('.ro-stale-unavailable').hidden).toBe(false);
        expect(part('.ro-stale-retry').hidden).toBe(true);
        expect(part('.ro-stale-reload').hidden).toBe(false);
    });

    test('a pending grace cannot repaint the terminal banner back to recoverable', () => {
        vi.useFakeTimers();

        markLiveStale();
        markLiveUnavailable();
        vi.advanceTimersByTime(LIVE_STALE_GRACE_MS * 2);

        expect(part('.ro-stale-unavailable').hidden).toBe(false);
        expect(part('.ro-stale-reload').hidden).toBe(false);
    });

    test('a committed snapshot clears the terminal state and restores the copy', () => {
        vi.useFakeTimers();
        markLiveUnavailable();

        clearLiveStale();

        expect(banner()).not.toBeVisible();
        expect(content()).not.toHaveClass('ro-stale');
        expect(content().dataset.roStale).toBeUndefined();
        expect(part('.bn-body:not(.ro-stale-unavailable)').hidden).toBe(false);
        expect(part('.ro-stale-unavailable').hidden).toBe(true);
        expect(part('.ro-stale-retry').hidden).toBe(false);
        expect(part('.ro-stale-reload').hidden).toBe(true);
        expect(vi.getTimerCount()).toBe(0);
    });
});

describe('independent owners', () => {
    test('clearing Live state cannot hide independently stale list data', () => {
        vi.useFakeTimers();

        markLiveStale();
        revealLiveStale();
        markListStale();

        clearLiveStale();
        expect(banner()).toBeVisible();
        expect(content()).toHaveClass('ro-stale');
        expect(content().dataset.roStale).toBeUndefined();

        clearListStale();
        expect(banner()).not.toBeVisible();
        expect(content()).not.toHaveClass('ro-stale');
        expect(vi.getTimerCount()).toBe(0);
    });

    test('a recovered list request cannot clear a Live disconnect', () => {
        vi.useFakeTimers();
        markListStale();
        markLiveStale();
        revealLiveStale();

        clearListStale();

        expect(banner()).toBeVisible();
        expect(content()).toHaveClass('ro-stale');
        expect(content().dataset.roStale).toBe('true');
    });

    test('a list morph that re-renders the banner keeps the terminal Live copy', () => {
        vi.useFakeTimers();
        markLiveUnavailable();

        // A `_table` morph re-renders the banner from the server template, so
        // the fresh node arrives in its shipped (recoverable, hidden) state.
        // Ownership lives in module state, not in serialized DOM attributes.
        renderStaleUI();
        clearListStale();

        expect(banner()).toBeVisible();
        expect(part('.ro-stale-unavailable').hidden).toBe(false);
        expect(part('.ro-stale-retry').hidden).toBe(true);
        expect(content()).toHaveClass('ro-stale');
    });
});
