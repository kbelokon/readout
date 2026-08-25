// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const reducedMotionQuery = '(prefers-reduced-motion: reduce)';

function mediaQueryList(matches: boolean, media = reducedMotionQuery): MediaQueryList {
    return {
        matches,
        media,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(() => true),
    } as unknown as MediaQueryList;
}

function stubMatchMedia(matches: boolean): ReturnType<typeof vi.fn> {
    const matchMedia = vi.fn((query: string) => mediaQueryList(matches, query));
    vi.stubGlobal('matchMedia', matchMedia);
    return matchMedia;
}

async function loadConfig(): Promise<void> {
    await import('./htmx-config.js');
}

beforeEach(() => {
    vi.resetModules();
});

afterEach(() => {
    vi.unstubAllGlobals();
});

describe('htmx configuration at module load', () => {
    test('loads safely without the htmx vendor and does not inspect motion preferences', async () => {
        const matchMedia = stubMatchMedia(false);
        vi.stubGlobal('htmx', undefined);

        await expect(loadConfig()).resolves.toBeUndefined();

        expect(matchMedia).not.toHaveBeenCalled();
    });

    test.each([
        { reducedMotion: false, globalViewTransitions: true },
        { reducedMotion: true, globalViewTransitions: false },
    ])(
        'sets globalViewTransitions=$globalViewTransitions when reduced motion is $reducedMotion',
        async ({ reducedMotion, globalViewTransitions }) => {
            const config = {
                globalViewTransitions: !globalViewTransitions,
                historyEnabled: true,
            };
            const htmx = { config };
            const matchMedia = stubMatchMedia(reducedMotion);
            vi.stubGlobal('htmx', htmx);

            await loadConfig();

            expect(matchMedia).toHaveBeenCalledExactlyOnceWith(reducedMotionQuery);
            expect(htmx.config).toBe(config);
            expect(config).toStrictEqual({
                globalViewTransitions,
                historyEnabled: true,
            });
        },
    );

    test('queries motion and writes the config before the import resolves', async () => {
        const order: string[] = [];
        let globalViewTransitions = false;
        const config = {
            get globalViewTransitions() {
                return globalViewTransitions;
            },
            set globalViewTransitions(value: boolean) {
                order.push(`write:${value}`);
                globalViewTransitions = value;
            },
        };
        const matchMedia = vi.fn((query: string) => {
            order.push(`query:${query}`);
            return mediaQueryList(false, query);
        });
        vi.stubGlobal('matchMedia', matchMedia);
        vi.stubGlobal('htmx', { config });

        await import('./htmx-config.js').then(() => {
            order.push('import:resolved');
        });

        expect(order).toStrictEqual([
            `query:${reducedMotionQuery}`,
            'write:true',
            'import:resolved',
        ]);
        expect(globalViewTransitions).toBe(true);
    });

    test('runs once per module instance and runs again only after resetModules', async () => {
        let reducedMotion = false;
        const config = { globalViewTransitions: false };
        const matchMedia = vi.fn((query: string) => mediaQueryList(reducedMotion, query));
        vi.stubGlobal('matchMedia', matchMedia);
        vi.stubGlobal('htmx', { config });

        await loadConfig();
        expect(config.globalViewTransitions).toBe(true);
        expect(matchMedia).toHaveBeenCalledOnce();

        reducedMotion = true;
        await loadConfig();
        expect(config.globalViewTransitions).toBe(true);
        expect(matchMedia).toHaveBeenCalledOnce();

        vi.resetModules();
        await loadConfig();
        expect(config.globalViewTransitions).toBe(false);
        expect(matchMedia).toHaveBeenCalledTimes(2);
    });
});
