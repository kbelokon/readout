// toasts.ts -- transient toast notifications (Unit 9 leaf migration; D24 /
// SPEC §8.8): bottom-right, 3.5s, mono caption voice. A toast exists ONLY for
// an async result detached from its trigger -- exactly two sanctioned triggers:
// the bulk download refused over the selection cap and "refresh resumed" after
// a failed-then-recovered auto-refresh (the polling layer calls window.roToast).
// Inline state changes (copy -> "Copied") stay inline, and there is deliberately
// NO "download ready" toast: the bulk download is a plain GET the browser
// handles, so no detached ready moment exists. The #ro-toasts host is layout
// chrome OUTSIDE every swap target, so an active toast survives list morphs.
//
// LEAF per the listener inventory: no document event listener at all -- it is a
// pure DOM-side-effect function the rest of the app calls (directly by name and
// via the window.roToast bridge the polling layer reaches).

const TOAST_VISIBLE_MS = 3500;
const TOAST_LEAVE_MS = 200;

export function showToast(message: string): void {
    const host = document.getElementById('ro-toasts');
    if (!host) {
        return;
    }
    const toast = document.createElement('div');
    toast.className = 'ro-toast';
    toast.textContent = message;
    host.appendChild(toast);
    window.setTimeout(() => {
        toast.classList.add('is-leaving');
        window.setTimeout(() => toast.remove(), TOAST_LEAVE_MS);
    }, TOAST_VISIBLE_MS);
}
