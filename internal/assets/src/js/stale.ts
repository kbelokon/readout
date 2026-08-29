// stale.ts -- stale data (auto-refresh failure) handling, migrated from
// legacy.js. CLIENT-SIDE, never blanks the rows (data never disappears). There is no server-side
// last-good cache: ordinary "stale" is the AUTO-REFRESH failure case, while
// degraded Live is an independent owner of the same warning surface. When the
// #resource-list-content morph-refresh request errors (htmx:responseError = a
// non-2xx reply, htmx:sendError = a transport failure), htmx does NOT swap on
// error, so the existing rows stay exactly as they were. We mark the content
// stale (a dim class) and reveal the pre-rendered hidden `.ro-banner.warn` so
// the user knows the data is last-known, not current. On the next successful
// refresh the morph swaps fresh rows and the afterSwap pipeline (orchestrated
// in legacy.js) clears the stale state via clearListStale. Pure DOM writes ->
// CSP-clean.
//
// The Go needle contract (internal/web/states_redesign_test.go) pins the FORMS
// preserved here: ro-stale-banner / ro-stale-retry / resource-list-content /
// ro:refresh / the literal gate `id === 'resource-list-content'` / the
// `htmx:responseError.{0,200}isListRefreshEvent` gate / the combined stale-owner
// visibility assignment / NO `innerHTML = ''` (the data-never-disappears law).

import { noteRefreshFailure, refreshNextAtMs } from './refresh.js';

// The one 1s ticker repainting "Retrying in Ns" while either ordinary refresh
// staleness or Live-unavailable state owns the visible banner.
let staleCountdownId: number | null = null;
// Ordinary refresh failure and Live fallback are independent owners of the same
// surface. An empty set is the natural fresh-page state; owner identity survives
// list morphs without mirroring runtime state into serialized DOM attributes.
const listStaleOwner = Symbol();
const liveUnavailableOwner = Symbol();
const staleOwners = new Set<symbol>();

interface BannerCopy {
    ariaLabel: string | null;
    buttonText: string;
    messageHTML: string;
    messageHidden: HTMLElement['hidden'];
    titleText: string;
}

interface BannerParts {
    button: HTMLElement;
    message: HTMLElement;
    title: HTMLElement;
}

function bannerElement(): HTMLElement | null {
    return document.querySelector('.ro-stale-banner') as HTMLElement | null;
}

// The banner is a closed server template. These three nodes are part of that
// component contract, just like the countdown span queried by its repaint path.
function bannerParts(banner: HTMLElement): BannerParts {
    return {
        button: banner.querySelector('[data-ro-action="retry"]') as HTMLElement,
        message: banner.querySelector('.bn-text') as HTMLElement,
        title: banner.querySelector('.bn-title') as HTMLElement,
    };
}

function rememberBannerCopy(banner: HTMLElement): BannerCopy {
    const serialized = banner.dataset.roStaleOriginalCopy;
    if (serialized) {
        try {
            return JSON.parse(serialized) as BannerCopy;
        } catch {}
    }
    const { button, message, title } = bannerParts(banner);
    const copy: BannerCopy = {
        ariaLabel: banner.getAttribute('aria-label'),
        buttonText: button.textContent as string,
        messageHTML: message.innerHTML,
        messageHidden: message.hidden,
        titleText: title.textContent as string,
    };
    banner.dataset.roStaleOriginalCopy = JSON.stringify(copy);
    return copy;
}

function restoreBannerCopy(banner: HTMLElement): void {
    const copy = rememberBannerCopy(banner);
    const { button, message, title } = bannerParts(banner);
    title.textContent = copy.titleText;
    message.innerHTML = copy.messageHTML;
    message.hidden = copy.messageHidden;
    button.textContent = copy.buttonText;
    if (copy.ariaLabel === null) banner.removeAttribute('aria-label');
    else banner.setAttribute('aria-label', copy.ariaLabel);
}

function stopStaleCountdown(): void {
    if (staleCountdownId !== null) {
        window.clearInterval(staleCountdownId);
        staleCountdownId = null;
    }
}

function startStaleCountdown(): void {
    if (staleCountdownId === null) {
        staleCountdownId = window.setInterval(updateStaleCountdown, 1000);
    }
    updateStaleCountdown();
}

function paintStaleState(): void {
    const listStale = staleOwners.has(listStaleOwner);
    const liveUnavailable = staleOwners.has(liveUnavailableOwner);
    document.getElementById('resource-list-content')?.classList.toggle('ro-stale', listStale);
    const banner = bannerElement();
    if (!banner) {
        stopStaleCountdown();
        return;
    }
    restoreBannerCopy(banner);
    if (liveUnavailable) {
        const { button, message, title } = bannerParts(banner);
        title.textContent = 'Live unavailable, polling ·';
        message.hidden = false;
        button.textContent = 'Retry';
    }
    banner.hidden = !(listStale || liveUnavailable);
    if (banner.hidden) stopStaleCountdown();
    else startStaleCountdown();
}

// updateStaleCountdown paints seconds-to-next-retry into the banner's
// [data-stale-countdown] span. The span is re-queried on every paint. With no
// retry armed (interval Off; the banner can still reveal when a user-initiated
// table request fails) the shipped "…" placeholder is restored.
export function updateStaleCountdown(): void {
    const banner = bannerElement();
    if (!banner) return;
    const span = banner.querySelector('[data-stale-countdown]');
    if (!span) {
        return;
    }
    const nextAt = refreshNextAtMs();
    if (!nextAt) {
        span.textContent = '…';
    } else {
        const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1000));
        span.textContent = `${remaining}s`;
    }
    if (staleOwners.has(liveUnavailableOwner)) {
        banner.setAttribute(
            'aria-label',
            `Live unavailable, polling. Retrying in ${span.textContent}. Retry`,
        );
    }
}

// True when the htmx event belongs to a request that lands in the live
// resource-list region: issued BY #resource-list-content (the refresh tick /
// retry) or TARGETING it (a user sort/filter partial). Guards so an unrelated
// boosted navigation error never dims the table.
export function isListRefreshEvent(event: Event): boolean {
    const detail = (event as CustomEvent).detail;
    if (!detail) {
        return false;
    }
    const elt = detail.elt;
    if (elt && elt.id === 'resource-list-content') {
        return true;
    }
    const target = detail.target;
    return !!target && target.id === 'resource-list-content';
}

export function markListStale(): void {
    staleOwners.add(listStaleOwner);
    // Live countdown for the banner's "Retrying in Ns" (the data-stale-countdown
    // hook). The immediate paint lands the right number before the ticker's
    // first 1s beat.
    paintStaleState();
}

export function markLiveUnavailable(): void {
    staleOwners.add(liveUnavailableOwner);
    paintStaleState();
}

export function clearLiveUnavailable(): void {
    staleOwners.delete(liveUnavailableOwner);
    paintStaleState();
}

export function clearListStale(): void {
    staleOwners.delete(listStaleOwner);
    paintStaleState();
}

// A non-2xx reply to the refresh GET: keep the rows (htmx does not swap on
// error), dim them, reveal the stale banner. The failure note FIRST: it re-aims
// the retry schedule, so the banner reveals with the countdown already pointing
// at the real next attempt.
document.addEventListener('htmx:responseError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
// A transport failure on the refresh GET: same stale treatment.
document.addEventListener('htmx:sendError', (event) => {
    if (isListRefreshEvent(event)) {
        noteRefreshFailure();
        markListStale();
    }
});
