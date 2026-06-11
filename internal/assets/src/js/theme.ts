// theme.ts -- the navbar theme-toggle POST target (Unit 9 leaf migration).
//
// The navbar theme toggle POSTs /preferences with a hidden `theme` value that
// must flip the EFFECTIVE palette. With an explicit choice (a theme cookie or
// ?theme=) the server already renders the correct opposite value AND pins
// data-theme on <html>, so we leave it alone (data-theme-explicit="true").
//
// With NO explicit choice (data-theme-explicit="false") the palette is driven
// by prefers-color-scheme, NOT the server theme.name -- under the dark default
// a cookieless light-OS user is theme.name="dark" server-side (so the server
// pre-fills theme="light") while their real palette is LIGHT, which would make
// the first click a no-op. So we derive the value here from the effective
// palette: post the OPPOSITE of matchMedia('(prefers-color-scheme: dark)'). The
// matching icon is chosen purely in CSS (both glyphs render); this only fixes
// the POST target, which CSS cannot reach. Pure CSP-clean DOM writes (no eval,
// no inline handler).
//
// This is a LEAF feature per the listener inventory: no inter-listener
// dependency. Its only event hook is the matchMedia `change` listener (NOT a
// document delegated event, so it is NOT a dispatcher binding) attached ONCE at
// module load; syncThemeTogglePostTarget is an idempotent INIT step the
// runInit chain in legacy.js re-runs on DOMContentLoaded + htmx:load.

const PREFERS_DARK = window.matchMedia('(prefers-color-scheme: dark)');

export function syncThemeTogglePostTarget(): void {
    const toggle = document.getElementById('btn-theme-toggle') as HTMLElement | null;
    if (!toggle) {
        return;
    }
    // Explicit choice -> the server value is authoritative; never override it.
    if (toggle.dataset.themeExplicit !== 'false') {
        return;
    }
    const form = (toggle as HTMLButtonElement).form;
    const input = form?.querySelector('input[name="theme"]');
    if (input) {
        // Effective palette is dark -> the toggle should switch to light, and
        // vice versa (post the opposite of the current effective scheme).
        (input as HTMLInputElement).value = PREFERS_DARK.matches ? 'light' : 'dark';
    }
}

// Re-derive the cookieless toggle target if the OS scheme changes while the page
// is open (so the no-cookie toggle keeps matching the live effective palette).
// Attached ONCE at module load -- not inside runInit -- so hx-boost re-init never
// stacks duplicate listeners. addEventListener is the modern matchMedia API
// (addListener is deprecated); the listener body is idempotent. The module is
// imported once by readout.ts, so this runs exactly once.
PREFERS_DARK.addEventListener('change', syncThemeTogglePostTarget);
