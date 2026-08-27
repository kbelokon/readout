// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const dependencies = vi.hoisted(() => ({
    captureRowModel: vi.fn(),
    virtualizePrepareSwap: vi.fn(),
}));

vi.mock('./filters.js', () => ({
    captureRowModel: dependencies.captureRowModel,
}));

vi.mock('./virtualizer.js', () => ({
    virtualizePrepareSwap: dependencies.virtualizePrepareSwap,
}));

interface MorphExtension {
    isInlineSwap(swapStyle: string): boolean;
    handleSwap(swapStyle: string, target: Element, fragment: DocumentFragment): boolean;
}

interface MorphCallbacks {
    beforeNodeMorphed?: (oldNode: Node) => boolean | undefined;
    afterNodeMorphed?: (oldNode: Node) => void;
}

interface VendorHarness {
    callbacks: MorphCallbacks;
    defineExtension: ReturnType<typeof vi.fn>;
    extension(): MorphExtension;
    matchMedia: ReturnType<typeof vi.fn>;
    morph: ReturnType<typeof vi.fn>;
}

function stubReducedMotion(matches: boolean): ReturnType<typeof vi.fn> {
    const matchMedia = vi.fn().mockImplementation((query: string) => ({
        matches,
        media: query,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
    }));
    vi.stubGlobal('matchMedia', matchMedia);
    return matchMedia;
}

function installVendors(reducedMotion = false): VendorHarness {
    const matchMedia = stubReducedMotion(reducedMotion);

    let registered: MorphExtension | undefined;
    const defineExtension = vi.fn((_name: string, extension: MorphExtension) => {
        registered = extension;
    });
    const callbacks: MorphCallbacks = {};
    const morph = vi.fn(() => true);

    vi.stubGlobal('htmx', { defineExtension });
    vi.stubGlobal('Idiomorph', {
        defaults: { callbacks },
        morph,
    });

    return {
        callbacks,
        defineExtension,
        extension: () => {
            expect(registered).toBeDefined();
            return registered as MorphExtension;
        },
        matchMedia,
        morph,
    };
}

async function importMorph(): Promise<void> {
    await import('./morph.js');
}

beforeEach(() => {
    vi.resetModules();
});

afterEach(() => {
    vi.unstubAllGlobals();
});

describe('ro-morph vendor guards and extension', () => {
    test('loads when both optional vendor globals are absent', async () => {
        await expect(importMorph()).resolves.toBeUndefined();
    });

    test('does not register a half-working extension without Idiomorph', async () => {
        const defineExtension = vi.fn();
        vi.stubGlobal('htmx', { defineExtension });

        await expect(importMorph()).resolves.toBeUndefined();

        expect(defineExtension).not.toHaveBeenCalled();
    });

    test('ignores an incomplete Idiomorph global', async () => {
        const defineExtension = vi.fn();
        const callbacks: MorphCallbacks = {};
        stubReducedMotion(false);
        vi.stubGlobal('htmx', { defineExtension });
        vi.stubGlobal('Idiomorph', { defaults: { callbacks } });

        await expect(importMorph()).resolves.toBeUndefined();

        expect(callbacks.beforeNodeMorphed).toBeUndefined();
        expect(callbacks.afterNodeMorphed).toBeUndefined();
        expect(defineExtension).not.toHaveBeenCalled();
    });

    test('installs callbacks but does not throw when htmx is absent or incomplete', async () => {
        for (const htmxVendor of [undefined, {}]) {
            vi.resetModules();
            vi.unstubAllGlobals();
            const matchMedia = stubReducedMotion(false);
            const callbacks: MorphCallbacks = {};
            vi.stubGlobal('Idiomorph', { defaults: { callbacks }, morph: vi.fn() });
            if (htmxVendor !== undefined) {
                vi.stubGlobal('htmx', htmxVendor);
            }

            await expect(importMorph()).resolves.toBeUndefined();

            expect(callbacks.beforeNodeMorphed).toBeTypeOf('function');
            expect(callbacks.afterNodeMorphed).toBeTypeOf('function');
            expect(matchMedia).toHaveBeenCalledExactlyOnceWith('(prefers-reduced-motion: reduce)');
        }
    });

    test.each([
        ['no defaults', { morph: vi.fn() }],
        ['no callbacks', { defaults: {}, morph: vi.fn() }],
    ])('registers with partial Idiomorph defaults: %s', async (_case, idiomorphVendor) => {
        const defineExtension = vi.fn();
        const matchMedia = stubReducedMotion(false);
        vi.stubGlobal('htmx', { defineExtension });
        vi.stubGlobal('Idiomorph', idiomorphVendor);

        await expect(importMorph()).resolves.toBeUndefined();

        expect(defineExtension).toHaveBeenCalledOnce();
        expect(matchMedia).not.toHaveBeenCalled();
    });

    test('registers ro-morph and declines non-morph swaps', async () => {
        const vendor = installVendors();
        await importMorph();

        expect(vendor.defineExtension).toHaveBeenCalledExactlyOnceWith('ro-morph', {
            isInlineSwap: expect.any(Function),
            handleSwap: expect.any(Function),
        });
        expect(vendor.matchMedia).toHaveBeenCalledExactlyOnceWith(
            '(prefers-reduced-motion: reduce)',
        );
        const extension = vendor.extension();
        expect(extension.isInlineSwap('morph')).toBe(true);
        expect(extension.isInlineSwap('innerHTML')).toBe(false);

        const target = document.createElement('div');
        const fragment = document.createDocumentFragment();
        expect(extension.handleSwap('innerHTML', target, fragment)).toBe(false);
        expect(dependencies.captureRowModel).not.toHaveBeenCalled();
        expect(dependencies.virtualizePrepareSwap).not.toHaveBeenCalled();
        expect(vendor.morph).not.toHaveBeenCalled();
    });

    test('morphs non-list targets without list-model side effects and returns vendor result', async () => {
        const vendor = installVendors();
        vendor.morph.mockReturnValueOnce(false);
        await importMorph();
        const extension = vendor.extension();

        const target = document.createElement('section');
        target.id = 'details';
        const fragment = document.createDocumentFragment();
        fragment.append(document.createElement('p'));

        expect(extension.handleSwap('morph', target, fragment)).toBe(false);
        expect(dependencies.captureRowModel).not.toHaveBeenCalled();
        expect(dependencies.virtualizePrepareSwap).not.toHaveBeenCalled();
        expect(vendor.morph).toHaveBeenCalledExactlyOnceWith(target, fragment.children, {
            morphStyle: 'innerHTML',
            ignoreActiveValue: true,
        });
    });

    test('captures the complete model before preparing virtualization and morphs exactly', async () => {
        const vendor = installVendors();
        await importMorph();
        const extension = vendor.extension();

        const target = document.createElement('div');
        target.id = 'resource-list-content';
        const fragment = document.createDocumentFragment();
        fragment.append(document.createElement('table'));

        expect(extension.handleSwap('morph', target, fragment)).toBe(true);
        expect(dependencies.captureRowModel).toHaveBeenCalledExactlyOnceWith(fragment);
        expect(dependencies.virtualizePrepareSwap).toHaveBeenCalledExactlyOnceWith(fragment);
        expect(dependencies.captureRowModel.mock.invocationCallOrder[0]).toBeLessThan(
            dependencies.virtualizePrepareSwap.mock.invocationCallOrder[0] as number,
        );
        expect(vendor.morph).toHaveBeenCalledExactlyOnceWith(target, fragment.children, {
            morphStyle: 'innerHTML',
            ignoreActiveValue: true,
        });
    });
});

describe('changed-cell flash', () => {
    test('flashes only TD elements whose text changed during the morph', async () => {
        const vendor = installVendors();
        await importMorph();

        const onBeforeNodeMorphed = vendor.callbacks.beforeNodeMorphed;
        const onAfterNodeMorphed = vendor.callbacks.afterNodeMorphed;
        expect(onBeforeNodeMorphed).toBeTypeOf('function');
        expect(onAfterNodeMorphed).toBeTypeOf('function');

        const changed = document.createElement('td');
        changed.textContent = 'Pending';
        expect(onBeforeNodeMorphed?.(changed)).toBeUndefined();
        changed.textContent = 'Running';
        onAfterNodeMorphed?.(changed);
        expect(changed).toHaveClass('ro-cell-changed');

        const unchanged = document.createElement('td');
        unchanged.textContent = 'Ready';
        onBeforeNodeMorphed?.(unchanged);
        onAfterNodeMorphed?.(unchanged);
        expect(unchanged).not.toHaveClass('ro-cell-changed');

        const nonCell = document.createElement('div');
        nonCell.textContent = 'before';
        Object.defineProperty(nonCell, 'textContent', {
            configurable: true,
            get: () => {
                throw new Error('non-cell subtree was read');
            },
        });
        expect(() => onBeforeNodeMorphed?.(nonCell)).not.toThrow();
        expect(() => onAfterNodeMorphed?.(nonCell)).not.toThrow();
        expect(nonCell).not.toHaveClass('ro-cell-changed');
    });

    test('does not flash a TD without a matching before callback', async () => {
        const vendor = installVendors();
        await importMorph();

        const cell = document.createElement('td');
        cell.textContent = 'Already morphed';
        vendor.callbacks.afterNodeMorphed?.(cell);

        expect(cell).not.toHaveClass('ro-cell-changed');
    });

    test('consumes each before snapshot exactly once', async () => {
        const vendor = installVendors();
        await importMorph();

        const cell = document.createElement('td');
        cell.textContent = 'Queued';
        vendor.callbacks.beforeNodeMorphed?.(cell);
        cell.textContent = 'Running';
        vendor.callbacks.afterNodeMorphed?.(cell);
        expect(cell).toHaveClass('ro-cell-changed');

        cell.classList.remove('ro-cell-changed');
        cell.textContent = 'Done';
        vendor.callbacks.afterNodeMorphed?.(cell);
        expect(cell).not.toHaveClass('ro-cell-changed');
    });

    test('does not install flash callbacks for reduced-motion users', async () => {
        const vendor = installVendors(true);

        await importMorph();

        expect(vendor.callbacks.beforeNodeMorphed).toBeUndefined();
        expect(vendor.callbacks.afterNodeMorphed).toBeUndefined();
        expect(vendor.defineExtension).toHaveBeenCalledOnce();
        expect(vendor.matchMedia).toHaveBeenCalledExactlyOnceWith(
            '(prefers-reduced-motion: reduce)',
        );
    });
});
