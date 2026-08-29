// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

import {
    applyLiveRowDeletions,
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
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('0 selected');
        expect(document.getElementById('ro-bulkbar')).not.toHaveClass('is-open');
        expect(document.getElementById('ro-bulkbar')).toHaveAttribute('inert');
    });

    test('keeps painting the bulk bar when its optional count label is absent', () => {
        document.getElementById('ro-bulk-count')?.remove();

        expect(() => rowState().setSelected('prod/default/api-0', true)).not.toThrow();
        expect(document.getElementById('ro-bulkbar')).toHaveClass('is-open');
        expect(document.getElementById('ro-bulk-download')).not.toBeDisabled();
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
        expect(row).toHaveClass('is-selected');
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('1 selected');
        expect(document.getElementById('ro-bulkbar')).toHaveClass('is-open');

        binding.handler(eventTarget(anchor), row);
        expect(rowState().selectedKeys()).toStrictEqual(['prod/default/api-0']);

        binding.handler(eventTarget(plain), row);
        expect(rowState().selectedKeys()).toStrictEqual([]);
        expect(row).not.toHaveClass('is-selected');
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('0 selected');
        expect(document.getElementById('ro-bulkbar')).not.toHaveClass('is-open');
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
        expect(download).toHaveAttribute('title', '');
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
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('0 selected');
        expect(document.getElementById('ro-bulkbar')).not.toHaveClass('is-open');
        expect(document.getElementById('ro-bulkbar')).toHaveAttribute('inert');
    });

    test('live deletions repaint only when they remove stored selection or focus', () => {
        rowState().setSelected('prod/default/api-0', true);
        rowState().setFocus('prod/default/db-0');
        const toggleAttribute = vi.spyOn(Element.prototype, 'toggleAttribute');
        toggleAttribute.mockClear();

        applyLiveRowDeletions(new Set(['prod/default/unknown']));

        expect(toggleAttribute).not.toHaveBeenCalled();
        expect(rowState().selectedKeys()).toStrictEqual(['prod/default/api-0']);
        expect(rowState().focusedKey()).toBe('prod/default/db-0');

        applyLiveRowDeletions(new Set(['prod/default/api-0']));

        expect(toggleAttribute).toHaveBeenCalled();
        expect(rowState().selectedKeys()).toStrictEqual([]);
        expect(rowState().focusedKey()).toBe('prod/default/db-0');

        toggleAttribute.mockClear();
        applyLiveRowDeletions(new Set(['prod/default/db-0']));

        expect(toggleAttribute).toHaveBeenCalled();
        expect(rowState().focusedKey()).toBeNull();
        expect(document.getElementById('ro-bulk-count')).toHaveTextContent('0 selected');
    });

    test('exports the delegated row-click contract', () => {
        expect(rowSelectionBindings).toHaveLength(1);
        expect(rowSelectionBindings[0].event).toBe('click');
        expect(rowSelectionBindings[0].selector).toBe('#resource-list-content tr[data-key]');
        expect(rowSelectionBindings[0].stop).toBeUndefined();
    });
});

describe('clipboard bridge', () => {
    beforeEach(() => {
        document.body.replaceChildren();
        Object.defineProperty(navigator, 'clipboard', {
            configurable: true,
            value: undefined,
        });
        Object.defineProperty(document, 'execCommand', {
            configurable: true,
            value: undefined,
        });
    });

    afterEach(() => {
        vi.restoreAllMocks();
    });

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

    test('uses an off-screen readonly textarea when the async API is unavailable', async () => {
        const select = vi.spyOn(HTMLTextAreaElement.prototype, 'select');
        let fallbackTextarea: HTMLTextAreaElement | null = null;
        let fallbackSnapshot:
            | {
                  value: string;
                  readOnly: boolean;
                  position: string;
                  top: string;
                  attached: boolean;
              }
            | undefined;
        const execCommand = vi.fn(() => {
            const textarea = document.querySelector('textarea');
            if (!textarea) {
                return false;
            }
            fallbackTextarea = textarea;
            fallbackSnapshot = {
                value: textarea.value,
                readOnly: textarea.readOnly,
                position: textarea.style.position,
                top: textarea.style.top,
                attached: document.body.contains(textarea),
            };
            return false;
        });
        Object.defineProperty(document, 'execCommand', {
            configurable: true,
            value: execCommand,
        });

        const ok = await new Promise<boolean>((resolve) => roCopyText('db-0', resolve));

        expect(ok).toBe(false);
        expect(execCommand).toHaveBeenCalledExactlyOnceWith('copy');
        expect(select).toHaveBeenCalledOnce();
        expect(fallbackTextarea).not.toBeNull();
        expect(fallbackSnapshot).toStrictEqual({
            value: 'db-0',
            readOnly: true,
            position: 'fixed',
            top: '-1000px',
            attached: true,
        });
        expect(document.querySelector('textarea')).not.toBeInTheDocument();
    });

    test('reports a throwing fallback as failed and still removes its textarea', async () => {
        const execCommand = vi.fn(() => {
            throw new Error('copy blocked');
        });
        Object.defineProperty(document, 'execCommand', {
            configurable: true,
            value: execCommand,
        });

        const ok = await new Promise<boolean>((resolve) => roCopyText('db-0', resolve));

        expect(ok).toBe(false);
        expect(execCommand).toHaveBeenCalledExactlyOnceWith('copy');
        expect(document.querySelector('textarea')).not.toBeInTheDocument();
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
