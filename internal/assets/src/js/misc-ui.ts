// misc-ui.ts -- the remaining leaf UI features (migrated from legacy.js): per-section
// YAML copy, section collapse + its on-load hash restore, the mobile sidebar
// hamburger, and the namespace dropdown (toggle / select / search-filter /
// enter-select). Each is LEAF per the listener inventory -- no inter-listener
// dependency:
//
//   - copy / section-fold sit in the monolith's big click listener AFTER the
//     fold-toggle branch; they resolve their section via closest('.collapsible')
//     and never co-match the migrated fold or gutter branches.
//   - the namespace dropdown's `.is-active` flag is read by keyboardSurfaceBusy()
//     (the still-resident gesture keydown's DOM-guard), so K3 stays inert while
//     the dropdown is open. That coupling is through the DOM STATE, not listener
//     order, so migrating these clicks to the dispatcher (registered first) keeps
//     the guard working byte-identically -- the flag is set on the same element
//     before any later keydown reads it.
//
// Branches that early-returned in the monolith become stop:true bindings. The
// section-collapse hash codec is split into a PURE parser (parseCollapsedNames)
// pinned by Vitest; the DOM application + the write path stay here.

import { parseCollapsedNames } from './collapse-hash.js';
import type { Binding } from './events.js';
import { roPrefsSetNamespace } from './prefs.js';
import { yamlCodeText } from './yaml-folds.js';

// parseCollapsedNames (the PURE read half of the collapse-hash codec) lives in
// collapse-hash.ts so it stays independently unit-testable; imported
// above and applied to the DOM here.

// collapseSectionsFromHash -- on load, collapse every section named in the URL
// fragment. Idempotent: adding `is-collapsed` to an already-collapsed section is
// a no-op. Idempotent runInit step consumed by legacy.js's runInit chain.
export function collapseSectionsFromHash(): void {
    parseCollapsedNames(document.location.hash).forEach((name) => {
        document
            .querySelectorAll(`main .collapsible[data-name="${CSS.escape(name)}"]`)
            .forEach((el) => {
                el.classList.add('is-collapsed');
            });
    });
}

// --- dispatcher bindings ---------------------------------------------------

export const miscBindings: Binding[] = [
    {
        event: 'click',
        selector: '[data-ro-action="toggle-sidebar"]',
        stop: true,
        handler: (event) => {
            event.preventDefault();
            const sidebar = document.querySelector('.ro-sidebar');
            if (sidebar) {
                sidebar.classList.toggle('is-active');
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: '[data-ro-action="copy"]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const copyBtn = matched as HTMLElement;
            const section = copyBtn.closest('.collapsible');
            const codeCell = section?.querySelector('.highlighttable td.code');
            const text = codeCell ? yamlCodeText(codeCell) : '';
            const label = copyBtn.querySelector('.ro-copy-text');
            const done = (ok: boolean) => {
                if (!label) {
                    return;
                }
                label.textContent = ok ? 'copied' : 'press ⌘C';
                window.setTimeout(() => {
                    label.textContent = 'copy';
                }, 1500);
            };
            if (navigator.clipboard?.writeText && text) {
                navigator.clipboard.writeText(text).then(
                    () => done(true),
                    () => done(false),
                );
            } else {
                done(false);
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: 'main .collapsible h4.title',
        stop: true,
        handler: (_event, matched) => {
            const section = (matched as Element).closest('.collapsible');
            if (!section) {
                return true;
            }
            section.classList.toggle('is-collapsed');
            const names: string[] = [];
            document.querySelectorAll('main .is-collapsed').forEach((el) => {
                const name = (el as HTMLElement).dataset.name;
                if (name !== undefined) {
                    names.push(name);
                }
            });
            if (names.length) {
                document.location.hash = `collapsed=${names.join(',')}`;
            } else {
                window.history.replaceState(
                    null,
                    document.title,
                    window.location.pathname + window.location.search,
                );
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: '#namespace-dropdown [data-ro-action="pick-namespace"]',
        stop: true,
        handler: (_event, matched) => {
            const href = (matched as Element).getAttribute('href');
            const hrefMatch = href
                ? /^\/clusters\/([^/]+)\/namespaces\/([^/]+)\//.exec(href)
                : null;
            if (hrefMatch) {
                roPrefsSetNamespace(
                    decodeURIComponent(hrefMatch[1]),
                    decodeURIComponent(hrefMatch[2]),
                );
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: '#namespace-dropdown .context-trigger',
        stop: true,
        handler: (_event, matched) => {
            const nsDropdown = (matched as Element).closest('#namespace-dropdown');
            if (!nsDropdown) {
                return true;
            }
            nsDropdown.classList.toggle('is-active');
            if (nsDropdown.classList.contains('is-active')) {
                const searchbox = document.getElementById('namespace-searchbox');
                if (searchbox) {
                    searchbox.focus();
                }
            }
            return true;
        },
    },
    {
        event: 'input',
        selector: '#namespace-searchbox',
        stop: true,
        handler: (_event, matched) => {
            const filterText = (matched as HTMLInputElement).value.toLowerCase();
            document.querySelectorAll('[data-ro-action="pick-namespace"]').forEach((element) => {
                const text = (element as HTMLElement).innerText.toLowerCase();
                if (text.indexOf(filterText) === -1) {
                    element.classList.add('is-hidden');
                } else {
                    element.classList.remove('is-hidden');
                }
            });
            return true;
        },
    },
    {
        event: 'keyup',
        selector: '#namespace-searchbox',
        stop: true,
        handler: (event) => {
            if ((event as KeyboardEvent).key !== 'Enter') {
                return true;
            }
            const firstVisible = Array.from(
                document.querySelectorAll('[data-ro-action="pick-namespace"]'),
            ).find((element) => !element.classList.contains('is-hidden'));
            (firstVisible as HTMLElement | undefined)?.click();
            return true;
        },
    },
    {
        event: 'click',
        selector: '[data-ro-more]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const chips = (matched as Element).closest('.ro-chips');
            if (chips) {
                const expanded = chips.classList.toggle('expanded');
                (matched as Element).setAttribute('aria-expanded', expanded ? 'true' : 'false');
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: '[data-ro-annolong]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const annoToggle = matched as HTMLElement;
            const pre = annoToggle.parentElement
                ? (annoToggle.parentElement.querySelector('.anno-pre') as HTMLElement | null)
                : null;
            if (pre) {
                const open = pre.hidden !== false; // hidden is "until-found" | boolean
                pre.hidden = !open;
                annoToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
                annoToggle.classList.toggle('open', open);
            }
            return true;
        },
    },
    {
        event: 'click',
        selector: '[data-ro-action="toggle-tools"]',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const toggle = matched as HTMLElement;
            toggle.classList.toggle('is-active');
            const targetEl = toggle.dataset.target
                ? document.getElementById(toggle.dataset.target)
                : null;
            if (targetEl) {
                targetEl.classList.toggle('is-active');
            }
            return true;
        },
    },
    {
        event: 'change',
        selector: 'input[data-ro-toggle-button]',
        stop: true,
        handler: (_event, matched) => {
            const buttonId = (matched as HTMLElement).dataset.roToggleButton;
            const button = buttonId ? document.getElementById(buttonId) : null;
            if (button) {
                const anyChecked =
                    document.querySelectorAll(`input[data-ro-toggle-button="${buttonId}"]:checked`)
                        .length > 0;
                (button as HTMLButtonElement).disabled = !anyChecked;
            }
            return true;
        },
    },
    {
        event: 'submit',
        selector: 'form.tools-form',
        handler: (_event, matched) => {
            const form = matched as HTMLFormElement;
            Array.prototype.slice
                .call(form.getElementsByTagName('input'))
                .forEach((input: HTMLInputElement) => {
                    if (input.name && !input.value) {
                        input.name = '';
                    }
                });
        },
    },
];
