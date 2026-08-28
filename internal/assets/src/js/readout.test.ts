import { beforeEach, expect, test, vi } from 'vitest';

const composition = vi.hoisted(() => {
    const bindings = [{ event: 'composed:event', handler: vi.fn() }];
    const order: string[] = [];
    return {
        bindings,
        order,
        registerBindings: vi.fn((received: unknown) => {
            order.push('registerBindings');
            void received;
        }),
    };
});

vi.mock('./htmx-config.js', () => {
    composition.order.push('htmx-config');
    return {};
});
vi.mock('./morph.js', () => {
    composition.order.push('morph');
    return {};
});
vi.mock('./bindings.js', () => {
    composition.order.push('bindings');
    return { bindings: composition.bindings };
});
vi.mock('./events.js', () => {
    composition.order.push('events');
    return { registerBindings: composition.registerBindings };
});
vi.mock('./init.js', () => {
    composition.order.push('init');
    return {};
});

beforeEach(() => {
    vi.resetModules();
    composition.order.length = 0;
});

test('evaluates the entry module graph once and registers the exact composed binding list', async () => {
    await import('./readout.js');
    await import('./readout.js');

    expect(composition.order).toStrictEqual([
        'htmx-config',
        'morph',
        'bindings',
        'events',
        'registerBindings',
        'init',
    ]);
    expect(composition.registerBindings).toHaveBeenCalledExactlyOnceWith(composition.bindings);
});
