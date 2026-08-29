// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

interface TransactionOptions {
    reconcile(): void;
    restoreExternalState(): void;
}

const seams = vi.hoisted(() => ({
    mode: 'success' as 'restore' | 'success',
    restoreThrew: false,
    throwFilterRestore: false,
    throwRowRestore: false,
    throwVirtualizerRestore: false,
    virtualizerActive: false,
    applyFilter: vi.fn(),
    applyRowDeletions: vi.fn(),
    reapplyRowState: vi.fn(),
    restoreFilter: vi.fn(),
    restoreRow: vi.fn(),
    restoreVirtualizer: vi.fn(),
    takeFilter: vi.fn(() => ({ applied: null })),
    takeRow: vi.fn(() => ({ selected: [], focus: null, bulkOverCapToasted: undefined })),
    takeVirtualizer: vi.fn(() => ({ state: {}, historyRecovery: null })),
    transaction: vi.fn(),
}));

vi.mock('./filters.js', () => ({
    applyLiveNameFilter: seams.applyFilter,
    takeLiveNameFilterCheckpoint: seams.takeFilter,
    restoreLiveNameFilterCheckpoint: seams.restoreFilter,
}));

vi.mock('./row-selection.js', () => ({
    applyLiveRowDeletions: seams.applyRowDeletions,
    reapplyRowState: seams.reapplyRowState,
    takeRowStateCheckpoint: seams.takeRow,
    restoreRowStateCheckpoint: seams.restoreRow,
}));

vi.mock('./virtualizer.js', () => ({
    virtualizerActive: () => seams.virtualizerActive,
    takeVirtualizerCheckpoint: seams.takeVirtualizer,
    restoreVirtualizerCheckpoint: seams.restoreVirtualizer,
}));

vi.mock('./list-projection.js', () => ({
    applyListProjectionDeltaTransaction: seams.transaction,
}));

import { applyLiveV2Delta, type LiveV2Cursor } from './live-protocol.js';

const cursor: LiveV2Cursor = {
    g: 'g',
    seq: 1,
    screen: '/pods',
    rev: 'r1',
    rv: '10',
    schema: 's',
};

function frame(withDelete = false): string {
    return JSON.stringify({
        v: 2,
        kind: 'delta',
        g: 'g',
        seq: 2,
        screen: '/pods',
        rev: 'r2',
        schema: 's',
        delta: {
            base: 'r1',
            rev: 'r2',
            ...(withDelete
                ? { remove: [{ key: 'gone', cause: 'delete' }], order: [] }
                : { order: ['kept'] }),
        },
    });
}

beforeEach(() => {
    vi.clearAllMocks();
    seams.mode = 'success';
    seams.restoreThrew = false;
    seams.throwFilterRestore = false;
    seams.throwRowRestore = false;
    seams.throwVirtualizerRestore = false;
    seams.virtualizerActive = false;
    seams.restoreFilter.mockImplementation(() => {
        if (seams.throwFilterRestore) throw new Error('filter restore failed');
    });
    seams.restoreRow.mockImplementation(() => {
        if (seams.throwRowRestore) throw new Error('row restore failed');
    });
    seams.restoreVirtualizer.mockImplementation(() => {
        if (seams.throwVirtualizerRestore) throw new Error('virtualizer restore failed');
    });
    seams.transaction.mockImplementation((_plan: unknown, options: TransactionOptions) => {
        if (seams.mode === 'restore') {
            try {
                options.restoreExternalState();
            } catch {
                seams.restoreThrew = true;
            }
            return {
                ok: false,
                error: {
                    code: seams.restoreThrew ? 'rollback-failed' : 'reconcile-failed',
                    message: 'synthetic transaction result',
                    fatal: seams.restoreThrew,
                },
            };
        }
        options.reconcile();
        return {
            ok: true,
            summary: {
                inserted: 0,
                updated: 0,
                deleted: 0,
                projected: 0,
                reordered: true,
                regions: [],
            },
        };
    });
});

describe('Live protocol external-state seams', () => {
    test.each(['virtualizer', 'filter', 'row'] as const)(
        'attempts every independent restore when the %s restore fails',
        (failedRestore) => {
            seams.mode = 'restore';
            seams.throwVirtualizerRestore = failedRestore === 'virtualizer';
            seams.throwFilterRestore = failedRestore === 'filter';
            seams.throwRowRestore = failedRestore === 'row';

            expect(applyLiveV2Delta(frame(true), cursor)).toMatchObject({
                ok: false,
                error: { code: 'rollback-failed', fatal: true },
            });
            expect(seams.restoreVirtualizer).toHaveBeenCalledTimes(1);
            expect(seams.restoreFilter).toHaveBeenCalledTimes(1);
            expect(seams.restoreRow).toHaveBeenCalledTimes(1);
        },
    );

    test.each([
        { active: false, rowReapplications: 1 },
        { active: true, rowReapplications: 0 },
    ])(
        'reconciles row state $rowReapplications time(s) when virtualizer active=$active',
        ({ active, rowReapplications }) => {
            seams.virtualizerActive = active;

            expect(applyLiveV2Delta(frame(), cursor).ok).toBe(true);
            expect(seams.applyFilter).toHaveBeenCalledTimes(1);
            expect(seams.reapplyRowState).toHaveBeenCalledTimes(rowReapplications);
        },
    );
});
