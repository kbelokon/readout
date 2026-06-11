// context-menu.ts -- the row right-click context menu (Unit 10, migrated from
// legacy.js). ONE server-rendered popover (#ro-ctxmenu); opening binds the
// right-clicked row's server-resolved targets onto the items' data-href and
// stashes the row name for Copy. Position is fixed + viewport-clamped.
//
// DISPATCH: this owns the right-click open (contextmenu), the menu-item
// activation + the UNCONDITIONAL dismiss step of the monolith's row-gesture
// click listener (C2 steps 1 & 2), and the Esc-closes-menu keydown (K2). The
// load-bearing intra-listener invariant (compound case 1): step 2's
// closeRowMenu runs UNCONDITIONALLY and the chain FALLS THROUGH (no stop) into
// bulk-actions' bulk steps and then row-selection's row-select step -- so a
// click on a DIFFERENT row while a menu is open dismisses the menu AND selects
// that row in one pass. The dismiss binding therefore carries NO stop; only the
// ctx-item branch (which RETURNED in the monolith) stops the chain. menu
// navigation goes through window.location.assign (the palette pattern) because
// htmx captures a boosted anchor's href at PROCESS time.

import type { Binding } from './events.js';
import { roCopyText, lastKeySegment } from './row-selection.js';

const CTX_CLAMP_W = 220;
const CTX_CLAMP_H = 240;

export function closeRowMenu(): void {
    const menu = document.getElementById('ro-ctxmenu');
    if (menu) {
        menu.classList.remove('is-open');
        menu.setAttribute('aria-hidden', 'true');
    }
}

export function openRowMenu(tr: HTMLElement, x: number, y: number): void {
    const menu = document.getElementById('ro-ctxmenu');
    if (!menu) {
        return;
    }
    const bind = (action: string, href: string): void => {
        const item = menu.querySelector('[data-ctx="' + action + '"]') as HTMLElement | null;
        if (!item) {
            return;
        }
        if (href) {
            item.dataset.href = href;
            item.hidden = false;
        } else {
            delete item.dataset.href;
            item.hidden = true; // e.g. View logs on a non-pod row
        }
    };
    bind('open', tr.dataset.href || '');
    bind('yaml', tr.dataset.yaml || '');
    bind('logs', tr.dataset.logs || '');
    bind('download', tr.dataset.download || '');
    (menu as HTMLElement).dataset.name = tr.dataset.name || lastKeySegment(tr.dataset.key || '');
    (menu as HTMLElement).style.left =
        Math.max(8, Math.min(x, window.innerWidth - CTX_CLAMP_W)) + 'px';
    (menu as HTMLElement).style.top =
        Math.max(8, Math.min(y, window.innerHeight - CTX_CLAMP_H)) + 'px';
    menu.classList.add('is-open');
    menu.setAttribute('aria-hidden', 'false');
}

// --- dispatcher bindings ----------------------------------------------------
export const contextMenuBindings: Binding[] = [
    // Right-click on an identity row opens the menu; anywhere else closes ours
    // and yields to the native menu.
    {
        event: 'contextmenu',
        handler: (event) => {
            const target = event.target as Element | null;
            const tr = target ? target.closest('#resource-list-content tr[data-key]') : null;
            if (!tr) {
                closeRowMenu();
                return;
            }
            event.preventDefault();
            const me = event as MouseEvent;
            openRowMenu(tr as HTMLElement, me.clientX, me.clientY);
        },
    },
    // C2 step 1: a context-menu item -> act, then close. Copy stays on the page;
    // the navigation items go through location.assign with the bound data-href.
    // Download YAML is a Content-Disposition attachment, so assigning it
    // downloads WITHOUT leaving the page. Returned in the monolith -> stop:true.
    {
        event: 'click',
        selector: '#ro-ctxmenu [data-ctx]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const item = matched as HTMLElement;
            const menu = item.closest('#ro-ctxmenu') as HTMLElement | null;
            const name = (menu && menu.dataset.name) || '';
            const href = item.dataset.href || '';
            closeRowMenu();
            if (item.dataset.ctx === 'copy') {
                roCopyText(name, () => {});
            } else if (href) {
                window.location.assign(href);
            }
            return true;
        },
    },
    // C2 step 2: ANY other click dismisses an open menu. UNCONDITIONAL and
    // NON-stopping -- the click then FALLS THROUGH to the bulk + row-select
    // bindings (bulk-actions.ts / row-selection.ts), so a click that lands on a
    // row both dismisses the menu AND toggles selection (compound case 1). No
    // selector (it runs on every click, like the monolith's step 2); closeRowMenu
    // on a closed menu is a no-op. NO stop: a stop here would silently drop the
    // selection while still passing a "menu closed" check.
    {
        event: 'click',
        handler: () => {
            closeRowMenu();
        },
    },
    // K2: Esc closes the context menu. Its own keydown branch (NO preventDefault),
    // idempotent (closeRowMenu on a closed menu is a no-op).
    {
        event: 'keydown',
        handler: (event) => {
            if ((event as KeyboardEvent).key === 'Escape') {
                closeRowMenu();
            }
        },
    },
];
