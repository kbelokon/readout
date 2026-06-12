// logs.ts -- the logs page leaf features (migrated from legacy.js):
// the Follow pin toggle, the timestamps/wrap display toggles, and the init pin.
//
// The .ro-logpre stream is its own scroll container (max-height + overflow in
// CSS) with the newest entries at the bottom. While the Follow toggle is active
// (the default "Following" state) the view pins to the tail: on page load and
// whenever the user re-activates the toggle. There is no client streaming -- the
// page refreshes via the form GET -- so "follow" means "start at, and snap back
// to, the tail". The Download-logs control is a REAL plain-GET <a> (no JS), so
// there is nothing to migrate for it here.
//
// LEAF per the listener inventory: no inter-listener dependency. The #logFollow
// click branch and the #logTs / #logWrap change branches lift verbatim into
// dispatcher bindings (each kept its monolith early-return as stop:true);
// initLogsFollow is an idempotent runInit step consumed by legacy.js.

import type { Binding } from './events.js';

// logsScrollToTail pins the log stream to its tail.
function logsScrollToTail(): void {
    const pre = document.querySelector('pre.ro-logpre');
    if (pre) {
        (pre as HTMLElement).scrollTop = pre.scrollHeight;
    }
}

// logsPinTailIfFollowing re-pins the stream to its tail when the Follow toggle
// is active. Beyond page init, the wrap/timestamps display toggles call it after
// their class flips: both reflow the stream (line heights and widths change),
// which would otherwise drift a followed tail mid-stream. Idempotent: re-pinning
// an already-pinned stream is a no-op, and pages without #logFollow bail.
function logsPinTailIfFollowing(): void {
    const follow = document.getElementById('logFollow');
    if (follow && !follow.classList.contains('quiet')) {
        logsScrollToTail();
    }
}

// initLogsFollow starts a logs page at the stream tail when the Follow toggle is
// active (it renders active by default). Idempotent runInit step.
export function initLogsFollow(): void {
    logsPinTailIfFollowing();
}

// --- dispatcher bindings ---------------------------------------------------

export const logsBindings: Binding[] = [
    // Logs Follow toggle: the active accent "Following" sticks the stream
    // to its tail; clicking flips to the quiet "Follow" (and back). Re-activating
    // snaps the stream to the tail immediately. Pure class + label flips -- no
    // request, the read-only floor is untouched. Kept its monolith early-return
    // (stop:true).
    {
        event: 'click',
        selector: '#logFollow',
        stop: true,
        handler: (_event, matched) => {
            const logFollow = matched as HTMLElement;
            const following = !logFollow.classList.toggle('quiet');
            logFollow.setAttribute('aria-pressed', following ? 'true' : 'false');
            const label = logFollow.querySelector('.follow-label');
            if (label) {
                label.textContent = following ? 'Following' : 'Follow';
            }
            if (following) {
                logsScrollToTail();
            }
            return true;
        },
    },
    // Logs display toggles: CLIENT-SIDE only, no refetch. The timestamps
    // checkbox shows/hides the .log-ts spans via the stream's `hide-ts` class.
    // Both flips reflow the stream, so while Following is active the tail is
    // re-pinned afterwards. The monolith #logTs branch early-returned (stop:true).
    {
        event: 'change',
        selector: '#logTs',
        stop: true,
        handler: (_event, matched) => {
            const logTs = matched as HTMLInputElement;
            const pre = document.querySelector('pre.ro-logpre');
            if (pre) {
                pre.classList.toggle('hide-ts', !logTs.checked);
                logsPinTailIfFollowing();
            }
            return true;
        },
    },
    // The wrap checkbox toggles `wrap` (pre-wrap + break-word). In the monolith
    // this was the LAST change branch (no branch follows it), so stop:true is the
    // faithful mirror.
    {
        event: 'change',
        selector: '#logWrap',
        stop: true,
        handler: (_event, matched) => {
            const logWrap = matched as HTMLInputElement;
            const pre = document.querySelector('pre.ro-logpre');
            if (pre) {
                pre.classList.toggle('wrap', logWrap.checked);
                logsPinTailIfFollowing();
            }
            return true;
        },
    },
];
