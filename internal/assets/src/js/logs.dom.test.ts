// @vitest-environment jsdom

import { beforeEach, describe, expect, test } from 'vitest';

import type { Binding } from './events.js';
import { initLogsFollow, logsBindings } from './logs.js';

function binding(selector: string): Binding {
    const found = logsBindings.find((item) => item.selector === selector);
    expect(found).toBeDefined();
    return found as Binding;
}

function renderLogs(quiet = false): {
    follow: HTMLElement;
    pre: HTMLElement;
    timestamps: HTMLInputElement;
    wrap: HTMLInputElement;
} {
    document.body.innerHTML = `
        <button id="logFollow" class="${quiet ? 'quiet' : ''}" aria-pressed="${quiet ? 'false' : 'true'}">
            <span class="follow-label">${quiet ? 'Follow' : 'Following'}</span>
        </button>
        <label><input id="logTs" type="checkbox" checked> Timestamps</label>
        <label><input id="logWrap" type="checkbox"> Wrap</label>
        <pre class="ro-logpre">line one\nline two\n</pre>
    `;
    const pre = document.querySelector('pre.ro-logpre') as HTMLElement;
    Object.defineProperty(pre, 'scrollHeight', { configurable: true, value: 640 });
    return {
        follow: document.getElementById('logFollow') as HTMLElement,
        pre,
        timestamps: document.getElementById('logTs') as HTMLInputElement,
        wrap: document.getElementById('logWrap') as HTMLInputElement,
    };
}

beforeEach(() => {
    renderLogs();
});

describe('logs follow mode', () => {
    test('exports the delegated logs event contract', () => {
        expect(
            logsBindings.map(({ event, selector, stop }) => ({ event, selector, stop })),
        ).toStrictEqual([
            { event: 'click', selector: '#logFollow', stop: true },
            { event: 'change', selector: '#logTs', stop: true },
            { event: 'change', selector: '#logWrap', stop: true },
        ]);
    });

    test('initializes an active stream at the tail and ignores quiet mode', () => {
        const { follow, pre } = renderLogs();
        initLogsFollow();
        expect(pre.scrollTop).toBe(640);

        pre.scrollTop = 10;
        follow.classList.add('quiet');
        initLogsFollow();
        expect(pre.scrollTop).toBe(10);
    });

    test('toggles label and aria, snapping only when follow is re-enabled', () => {
        const { follow, pre } = renderLogs();
        const followBinding = binding('#logFollow');
        pre.scrollTop = 17;

        expect(followBinding.handler(new MouseEvent('click'), follow)).toBe(true);
        expect(followBinding.stop).toBe(true);
        expect(follow).toHaveClass('quiet');
        expect(follow).toHaveAttribute('aria-pressed', 'false');
        expect(follow.querySelector('.follow-label')?.textContent).toBe('Follow');
        expect(pre.scrollTop).toBe(17);

        pre.scrollTop = 12;
        followBinding.handler(new MouseEvent('click'), follow);
        expect(follow).not.toHaveClass('quiet');
        expect(follow).toHaveAttribute('aria-pressed', 'true');
        expect(follow.querySelector('.follow-label')?.textContent).toBe('Following');
        expect(pre.scrollTop).toBe(640);
    });

    test('still toggles follow state when its optional label is absent', () => {
        const { follow } = renderLogs();
        follow.querySelector('.follow-label')?.remove();

        expect(() => binding('#logFollow').handler(new MouseEvent('click'), follow)).not.toThrow();
        expect(follow).toHaveClass('quiet');
        expect(follow).toHaveAttribute('aria-pressed', 'false');
    });
});

describe('logs display controls', () => {
    test('timestamps toggle the hide class and repin while following', () => {
        const { pre, timestamps } = renderLogs();
        timestamps.checked = false;
        pre.scrollTop = 20;

        expect(binding('#logTs').handler(new Event('change'), timestamps)).toBe(true);

        expect(pre).toHaveClass('hide-ts');
        expect(pre.scrollTop).toBe(640);

        timestamps.checked = true;
        binding('#logTs').handler(new Event('change'), timestamps);
        expect(pre).not.toHaveClass('hide-ts');
    });

    test('wrap toggles without moving a stream whose follow mode is quiet', () => {
        const { pre, wrap } = renderLogs(true);
        pre.scrollTop = 25;
        wrap.checked = true;

        expect(binding('#logWrap').handler(new Event('change'), wrap)).toBe(true);

        expect(pre).toHaveClass('wrap');
        expect(pre.scrollTop).toBe(25);
    });

    test('wrap repins a stream whose follow mode is active', () => {
        const { pre, wrap } = renderLogs();
        pre.scrollTop = 25;
        wrap.checked = true;

        expect(binding('#logWrap').handler(new Event('change'), wrap)).toBe(true);

        expect(pre).toHaveClass('wrap');
        expect(pre.scrollTop).toBe(640);
    });

    test('all operations degrade safely when the stream element is absent', () => {
        const { follow, timestamps, wrap } = renderLogs();
        document.querySelector('pre.ro-logpre')?.remove();

        expect(() => initLogsFollow()).not.toThrow();
        expect(() => binding('#logFollow').handler(new MouseEvent('click'), follow)).not.toThrow();
        expect(() => binding('#logTs').handler(new Event('change'), timestamps)).not.toThrow();
        expect(() => binding('#logWrap').handler(new Event('change'), wrap)).not.toThrow();
    });
});
