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
    morph: ReturnType<typeof vi.fn>;
}

function stubReducedMotion(matches: boolean): void {
    vi.stubGlobal(
        'matchMedia',
        vi.fn().mockImplementation((query: string) => ({
            matches,
            media: query,
            onchange: null,
            addEventListener: vi.fn(),
            removeEventListener: vi.fn(),
            addListener: vi.fn(),
            removeListener: vi.fn(),
            dispatchEvent: vi.fn(),
        })),
    );
}

function installVendors(reducedMotion = false): VendorHarness {
    stubReducedMotion(reducedMotion);

    let registered: MorphExtension | undefined;
    const defineExtension = vi.fn((name: string, extension: MorphExtension) => {
        expect(name).toBe('ro-morph');
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
    test('loads without Idiomorph and does not register a half-working extension', async () => {
        const defineExtension = vi.fn();
        vi.stubGlobal('htmx', { defineExtension });

        await expect(importMorph()).resolves.toBeUndefined();

        expect(defineExtension).not.toHaveBeenCalled();
    });

    test('registers ro-morph and declines non-morph swaps', async () => {
        const vendor = installVendors();
        await importMorph();

        expect(vendor.defineExtension).toHaveBeenCalledOnce();
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
        onBeforeNodeMorphed?.(nonCell);
        nonCell.textContent = 'after';
        onAfterNodeMorphed?.(nonCell);
        expect(nonCell).not.toHaveClass('ro-cell-changed');
    });

    test('does not install flash callbacks for reduced-motion users', async () => {
        const vendor = installVendors(true);

        await importMorph();

        expect(vendor.callbacks.beforeNodeMorphed).toBeUndefined();
        expect(vendor.callbacks.afterNodeMorphed).toBeUndefined();
        expect(vendor.defineExtension).toHaveBeenCalledOnce();
    });
});
