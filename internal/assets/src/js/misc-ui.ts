// misc-ui.ts -- the remaining leaf UI features (Unit 9 migration): per-section
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
// pinned by node:test; the DOM application + the write path stay here.

import type { Binding } from './events.js';
import { yamlCodeText } from './yaml-folds.js';
import { roPrefsSetNamespace } from './prefs.js';
import { parseCollapsedNames } from './collapse-hash.js';

// parseCollapsedNames (the PURE read half of the collapse-hash codec) lives in
// collapse-hash.ts so it stays node-testable (no runtime imports); imported
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
    // Mobile hamburger: a delegated click on `.menu-toggle` reveals/hides the
    // sidebar by toggling `.is-active` on `.ro-sidebar`. No-op when no sidebar is
    // present (e.g. the Clusters entry page). Kept its early-return (stop:true).
    {
        event: 'click',
        selector: '.menu-toggle',
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
    // .ro-copy-btn (per-section YAML copy): copy THIS section's raw YAML to the
    // clipboard via navigator.clipboard.writeText -- CSP-clean. The raw text is
    // read from the section's Pygments `td.code` cell (the gutter lives in a
    // separate `td.linenos`), with any injected fold controls stripped first
    // (yamlCodeText) so the copy is the full source YAML in any fold state. The
    // button briefly flips its label to "copied". Matched (and stop:true) BEFORE
    // the section-fold binding so a copy click never toggles the section fold.
    {
        event: 'click',
        selector: '.ro-copy-btn',
        stop: true,
        handler: (event, matched) => {
            event.preventDefault();
            const copyBtn = matched as HTMLElement;
            const section = copyBtn.closest('.collapsible');
            const codeCell = section && section.querySelector('.highlighttable td.code');
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
            if (navigator.clipboard && navigator.clipboard.writeText && text) {
                navigator.clipboard.writeText(text).then(() => done(true), () => done(false));
            } else {
                done(false);
            }
            return true;
        },
    },
    // .collapsible h4.title: toggle `is-collapsed` on the section and sync the
    // URL fragment (collapsed=<names>) with all currently-collapsed sections. The
    // section is resolved via closest('.collapsible') (NOT parentElement) so a
    // Unit-10 YAML card (h4.title nested in .ro-card-head) folds the right node.
    // Registered AFTER the copy binding (copy's stop:true short-circuits a copy
    // click), reproducing the monolith order. Kept its early-return (stop:true).
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
                window.history.replaceState(null, '', window.location.pathname + window.location.search);
            }
            return true;
        },
    },
    // Namespace switch (D9): picking a namespace in the topbar dropdown records it
    // as this cluster's last-used namespace in the ro_prefs cookie (server-read
    // only, for cluster-entry hrefs -- never a redirect). The click is
    // deliberately NOT prevented; the boosted navigation proceeds. The cookie
    // write rides the prefs.ts surface directly (the same seam legacy uses).
    // Kept its early-return (stop:true).
    {
        event: 'click',
        selector: '#namespace-dropdown .namespace-item',
        stop: true,
        handler: (_event, matched) => {
            const hrefMatch = /^\/clusters\/([^/]+)\/namespaces\/([^/]+)\//
                .exec((matched as Element).getAttribute('href') || '');
            if (hrefMatch) {
                roPrefsSetNamespace(
                    decodeURIComponent(hrefMatch[1]),
                    decodeURIComponent(hrefMatch[2]),
                );
            }
            return true;
        },
    },
    // #namespace-dropdown .context-trigger: toggle `is-active`; focus the
    // searchbox when opening. Kept its early-return (stop:true).
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
    // #namespace-searchbox input: filter the .namespace-item links by
    // case-insensitive substring. Terminal branch in the monolith input listener
    // (no branch followed it), reproduced as stop:true.
    {
        event: 'input',
        selector: '#namespace-searchbox',
        stop: true,
        handler: (_event, matched) => {
            const filterText = (matched as HTMLInputElement).value.toLowerCase();
            document.querySelectorAll('.namespace-item').forEach((element) => {
                const text = ((element as HTMLElement).innerText || '').toLowerCase();
                if (text.indexOf(filterText) === -1) {
                    element.classList.add('is-hidden');
                } else {
                    element.classList.remove('is-hidden');
                }
            });
            return true;
        },
    },
    // #namespace-searchbox keyup: Enter selects the first still-visible match.
    // Sole branch of the monolith keyup listener; stop:true mirrors its return.
    {
        event: 'keyup',
        selector: '#namespace-searchbox',
        stop: true,
        handler: (event) => {
            if ((event as KeyboardEvent).key !== 'Enter') {
                return true;
            }
            const elements = document.querySelectorAll('.namespace-item');
            for (let i = 0; i < elements.length; i++) {
                if (!elements[i].classList.contains('is-hidden')) {
                    (elements[i] as HTMLElement).click();
                    break;
                }
            }
            return true;
        },
    },
];
