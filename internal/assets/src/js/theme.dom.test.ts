// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

interface MediaHarness {
    media: MediaQueryList;
    setMatches(matches: boolean): void;
    listener: ReturnType<typeof vi.fn>;
}

function installMatchMedia(initial: boolean): MediaHarness {
    let matches = initial;
    const listener = vi.fn();
    const media = {
        get matches() {
            return matches;
        },
        media: '(prefers-color-scheme: dark)',
        onchange: null,
        addEventListener: listener,
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(() => true),
    } as unknown as MediaQueryList;
    Object.defineProperty(window, 'matchMedia', {
        configurable: true,
        value: vi.fn(() => media),
    });
    return {
        media,
        setMatches(next) {
            matches = next;
        },
        listener,
    };
}

async function loadTheme(matches: boolean) {
    const harness = installMatchMedia(matches);
    vi.resetModules();
    const module = await import('./theme.js');
    return { ...harness, ...module };
}

function renderToggle(explicit: string, value = 'server-value'): HTMLInputElement {
    document.body.innerHTML = `
        <form>
            <button id="btn-theme-toggle" data-theme-explicit="${explicit}"></button>
            <input name="theme" value="${value}">
        </form>
    `;
    return document.querySelector('input[name="theme"]') as HTMLInputElement;
}

describe('theme toggle target', () => {
    beforeEach(() => {
        document.body.replaceChildren();
    });

    test('registers exactly one OS-scheme listener at module load', async () => {
        const { listener, syncThemeTogglePostTarget } = await loadTheme(false);

        expect(listener).toHaveBeenCalledOnce();
        expect(listener).toHaveBeenCalledWith('change', syncThemeTogglePostTarget);
    });

    test('leaves the server target untouched for an explicit theme', async () => {
        const input = renderToggle('true');
        const { syncThemeTogglePostTarget } = await loadTheme(true);

        syncThemeTogglePostTarget();

        expect(input).toHaveValue('server-value');
    });

    test.each([
        { prefersDark: true, target: 'light' },
        { prefersDark: false, target: 'dark' },
    ])(
        'posts the opposite of the effective OS palette: $target',
        async ({ prefersDark, target }) => {
            const input = renderToggle('false');
            const { syncThemeTogglePostTarget } = await loadTheme(prefersDark);

            syncThemeTogglePostTarget();

            expect(input).toHaveValue(target);
        },
    );

    test('recomputes the target when the OS preference changes', async () => {
        const input = renderToggle('false');
        const harness = await loadTheme(false);
        harness.syncThemeTogglePostTarget();
        expect(input).toHaveValue('dark');

        harness.setMatches(true);
        const callback = harness.listener.mock.calls[0][1] as () => void;
        callback();

        expect(input).toHaveValue('light');
    });

    test('is safe when the toggle or its form input is absent', async () => {
        const { syncThemeTogglePostTarget } = await loadTheme(false);

        expect(() => syncThemeTogglePostTarget()).not.toThrow();
        document.body.innerHTML =
            '<button id="btn-theme-toggle" data-theme-explicit="false"></button>';
        expect(() => syncThemeTogglePostTarget()).not.toThrow();
    });
});
