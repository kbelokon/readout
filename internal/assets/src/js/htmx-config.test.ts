// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

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
    test('loads safely without the htmx vendor', async () => {
        vi.stubGlobal('htmx', undefined);

        await expect(loadConfig()).resolves.toBeUndefined();
    });

    test('disables native transitions before the import resolves and preserves other config', async () => {
        const order: string[] = [];
        let globalViewTransitions = true;
        const config = {
            historyEnabled: true,
            get globalViewTransitions() {
                return globalViewTransitions;
            },
            set globalViewTransitions(value: boolean) {
                order.push(`write:${value}`);
                globalViewTransitions = value;
            },
        };
        vi.stubGlobal('htmx', { config });

        await import('./htmx-config.js').then(() => {
            order.push('import:resolved');
        });

        expect(order).toStrictEqual(['write:false', 'import:resolved']);
        expect(globalViewTransitions).toBe(false);
        expect(config.historyEnabled).toBe(true);
    });

    test('runs once per module instance and runs again only after resetModules', async () => {
        const writes: boolean[] = [];
        let globalViewTransitions = true;
        const config = {
            get globalViewTransitions() {
                return globalViewTransitions;
            },
            set globalViewTransitions(value: boolean) {
                writes.push(value);
                globalViewTransitions = value;
            },
        };
        vi.stubGlobal('htmx', { config });

        await loadConfig();
        expect(config.globalViewTransitions).toBe(false);
        expect(writes).toStrictEqual([false]);

        globalViewTransitions = true;
        await loadConfig();
        expect(config.globalViewTransitions).toBe(true);
        expect(writes).toStrictEqual([false]);

        vi.resetModules();
        await loadConfig();
        expect(config.globalViewTransitions).toBe(false);
        expect(writes).toStrictEqual([false, false]);
    });
});
