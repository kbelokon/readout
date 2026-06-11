// skeleton.ts -- the loading skeleton (D16 / SPEC §7.19), migrated from
// legacy.js. Shown ONLY into an EMPTY swap target. Every full page is
// server-rendered with rows already in place, and the morph refresh keeps the
// last-good rows, so the only valid skeleton moment is a partial request
// landing in a BLANK #resource-list-content. A POPULATED region NEVER gets a
// skeleton over its content (the data-never-disappears law). The rows are
// cloned from the inert server-baked #ro-skel-template sibling -- pure DOM,
// CSP-clean.
//
// The Go needle contract (internal/web/states_redesign_test.go) pins the FORMS
// preserved here: ro-skel-template / listRegionIsEmpty / childElementCount ===
// 0 / clearListSkeleton / the polarity gate
// `|| !listRegionIsEmpty(content)) { return`.

import { isListRefreshEvent } from './stale.js';

// True when the list region is a BLANK region: zero element children. The probe
// is element-count, not a selector list, because ANY existing content is
// something the skeleton clone (replaceChildren) would wipe -- a selector
// denylist once misclassified a banner-only region as empty and the clone
// destroyed the only visible diagnostic.
function listRegionIsEmpty(content: Element): boolean {
    return content.childElementCount === 0;
}

document.addEventListener('htmx:beforeRequest', (event) => {
    if (!isListRefreshEvent(event)) {
        return;
    }
    const content = document.getElementById('resource-list-content');
    const template = document.getElementById('ro-skel-template') as HTMLTemplateElement | null;
    if (!content || !template || !listRegionIsEmpty(content)) {
        return;
    }
    content.replaceChildren(...Array.from(template.children, (node) => node.cloneNode(true)));
});

// A failed request into a skeleton-only region removes the skeleton (htmx does
// not swap on error), so the region returns to empty instead of shimmering
// forever. Last-good rows are never involved: the skeleton only ever existed in
// a region that had none.
function clearListSkeleton(): void {
    const content = document.getElementById('resource-list-content');
    const skel = content?.querySelector(':scope > .ro-skel');
    if (skel) {
        skel.remove();
    }
}
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        clearListSkeleton();
    }
});
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        clearListSkeleton();
    }
});
