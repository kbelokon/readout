// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import {
    BULK_NAMES_MAX,
    clearRowState,
    lastKeySegment,
    reapplyRowState,
    roCopyText,
    rowSelectionBindings,
} from './row-selection.js';

interface RowState {
    setSelected(key: string, on: boolean): void;
    setFocus(key: string): void;
    focusedKey(): string | null;
    clear(): void;
    selectedKeys(): string[];
    selectedEntries(): { key: string; name: string }[];
}

function rowState(): RowState {
    return (window as unknown as { roRowState: RowState }).roRowState;
}

function renderRows(): void {
    document.body.innerHTML = `
        <div id="resource-list-content">
            <div class="ro-table-wrap" tabindex="0">
                <table><tbody>
                    <tr id="row-a" data-key="prod/default/api-0" data-name="api-full-name">
                        <td><span class="plain">api</span><a href="/api">open</a></td>
                    </tr>
                    <tr id="row-b" data-key="prod/default/db-0"><td>db</td></tr>
                </tbody></table>
            </div>
        </div>
        <div id="ro-bulkbar" data-bulk-href="/bulk?format=yaml" inert>
            <span id="ro-bulk-count"></span>
            <button id="ro-bulk-download"></button>
        </div>
    `;
}

function eventTarget(target: Element): MouseEvent {
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'target', { configurable: true, value: target });
    return event;
}

describe('row selection state', () => {
    beforeEach(() => {
        renderRows();
        clearRowState();
        delete (window as unknown as { roToast?: unknown }).roToast;
    });

    test('extracts the object name from an identity key', () => {
        expect(lastKeySegment('prod/default/api-0')).toBe('api-0');
        expect(lastKeySegment('')).toBe('');
        expect(lastKeySegment('prod/default/')).toBe('');
    });

    test('selection paints the row, count, visibility, and inert state', () => {
        rowState().setSelected('prod/default/api-0', true);

        expect(document.getElementById('row-a')).toHaveClass('is-selected');
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('1 selected');
        expect(document.getElementById('ro-bulkbar')).toHaveClass('is-open');
        expect(document.getElementById('ro-bulkbar')).not.toHaveAttribute('inert');

        rowState().setSelected('prod/default/api-0', false);

        expect(document.getElementById('row-a')).not.toHaveClass('is-selected');
        expect(document.getElementById('ro-bulkbar')).not.toHaveClass('is-open');
        expect(document.getElementById('ro-bulkbar')).toHaveAttribute('inert');
    });

    test('keeps the full captured name after a selected row leaves the DOM', () => {
        rowState().setSelected('prod/default/api-0', true);
        document.getElementById('row-a')?.remove();

        expect(rowState().selectedEntries()).toStrictEqual([
            { key: 'prod/default/api-0', name: 'api-full-name' },
        ]);
    });

    test('falls back to the key tail when selection is set off-DOM', () => {
        rowState().setSelected('prod/default/missing-0', true);

        expect(rowState().selectedEntries()).toStrictEqual([
            { key: 'prod/default/missing-0', name: 'missing-0' },
        ]);
    });

    test('reapplies focus and aria-activedescendant after a morph', () => {
        rowState().setFocus('prod/default/db-0');
        const wrap = document.querySelector('.ro-table-wrap');

        expect(document.getElementById('row-b')).toHaveClass('kfocus');
        expect(wrap).toHaveAttribute('aria-activedescendant', 'row-b');

        document.getElementById('row-b')?.remove();
        reapplyRowState();

        expect(rowState().focusedKey()).toBe('prod/default/db-0');
        expect(wrap).not.toHaveAttribute('aria-activedescendant');
    });

    test('plain row clicks toggle selection while interactive descendants do not', () => {
        const binding = rowSelectionBindings[0];
        const row = document.getElementById('row-a') as HTMLElement;
        const plain = row.querySelector('.plain') as HTMLElement;
        const anchor = row.querySelector('a') as HTMLAnchorElement;

        binding.handler(eventTarget(plain), row);
        expect(rowState().selectedKeys()).toStrictEqual(['prod/default/api-0']);

        binding.handler(eventTarget(anchor), row);
        expect(rowState().selectedKeys()).toStrictEqual(['prod/default/api-0']);

        binding.handler(eventTarget(plain), row);
        expect(rowState().selectedKeys()).toStrictEqual([]);
    });

    test('enforces the bulk cap and rearms its toast after dropping under it', () => {
        const toast = vi.fn();
        (window as unknown as { roToast: typeof toast }).roToast = toast;

        for (let i = 0; i <= BULK_NAMES_MAX; i++) {
            rowState().setSelected(`prod/default/item-${i}`, true);
        }

        const download = document.getElementById('ro-bulk-download');
        expect(download).toBeDisabled();
        expect(download).toHaveAttribute(
            'title',
            `Over the ${BULK_NAMES_MAX}-object bulk download cap`,
        );
        expect(toast).toHaveBeenCalledOnce();
        expect(toast).toHaveBeenCalledWith(
            `Download refused: ${BULK_NAMES_MAX + 1} selected (max ${BULK_NAMES_MAX})`,
        );

        rowState().setSelected('prod/default/item-0', false);
        expect(download).not.toBeDisabled();
        rowState().setSelected('prod/default/new-item', true);
        expect(toast).toHaveBeenCalledTimes(2);
    });

    test('clear removes selection and focus together', () => {
        rowState().setSelected('prod/default/api-0', true);
        rowState().setFocus('prod/default/api-0');

        rowState().clear();

        expect(rowState().selectedKeys()).toStrictEqual([]);
        expect(rowState().focusedKey()).toBeNull();
        expect(document.getElementById('row-a')).not.toHaveClass('is-selected', 'kfocus');
    });
});

describe('clipboard bridge', () => {
    test('uses the async clipboard API when available', async () => {
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: { writeText },
        });

        const ok = await new Promise<boolean>((resolve) => roCopyText('api-0', resolve));

        expect(ok).toBe(true);
        expect(writeText).toHaveBeenCalledWith('api-0');
    });

    test('falls back to execCommand after an async clipboard rejection', async () => {
        Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: { writeText: vi.fn().mockRejectedValue(new Error('denied')) },
        });
        const execCommand = vi.fn(() => true);
        Object.defineProperty(document, 'execCommand', {
            configurable: true,
            value: execCommand,
        });

        const ok = await new Promise<boolean>((resolve) => roCopyText('db-0', resolve));

        expect(ok).toBe(true);
        expect(execCommand).toHaveBeenCalledWith('copy');
        expect(document.querySelector('textarea')).not.toBeInTheDocument();
    });
});
