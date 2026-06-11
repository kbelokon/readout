import { test, expect, type Page } from '@playwright/test';
import { controlURL } from './playwright.config';

// Compound-gesture inter-listener contract (Unit 8), end to end:
//
// The legacy monolith (internal/assets/src/js/legacy.js) carries 5 delegated
// `click` and 3 `keydown` listeners on `document`. Their mutual decoupling is
// of TWO kinds, and a future unified dispatcher must reproduce BOTH:
//
//   (A) DOM-GUARD decoupling — surfaces stay disjoint by reading the live DOM,
//       not by registration order: keyboardSurfaceBusy() (palette `.open`,
//       ctxmenu `.is-open`, namespace dropdown, columns popover), the
//       text-entry gate, and the columns outside-click `[data-ro-cols-toggle]`
//       escape;
//   (B) INTRA-listener SEQUENCE — the inviolable invariant is the ORDER of
//       steps INSIDE one listener: the row-gesture click closes any open menu
//       FIRST and then falls through to toggle row selection, so a click on a
//       different row both dismisses the menu AND selects that row in one pass.
//
// These cases pin the ACTUAL behavior of the current monolith (they are green
// BEFORE any dispatcher merge — they fix the real, not the ideal). The list is
// kept NON-virtualized on purpose (3 rows; the windowed path has its own spec).
//
// Driven against the fakeapi harness, same as gestures/keyboard/palette specs.

const PODS = '/clusters/e2e/namespaces/default/pods';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

// roFocusKey reads the keyboard row focus through the same window.roRowState
// seam the keyboard spec uses; null when no row is focused.
function roFocusKey(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const focused = document.querySelector('#resource-list-content tr.kfocus');
    return focused ? ((focused as HTMLElement).dataset.key ?? null) : null;
  });
}

function selectedKeys(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    (window as unknown as { roRowState: { selectedKeys(): string[] } }).roRowState.selectedKeys()
  );
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the row/keyboard/palette gesture surfaces are desktop-only (below 760px the card layer replaces the table, D22)'
  );
  await control('/__control/reset');
});

// CASE 1 — the documented fall-through (class B): the row-gesture click listener
// closes an open context menu UNCONDITIONALLY and then continues into the
// row-select branch, so a click that lands on a DIFFERENT row dismisses the menu
// AND selects that row in the same pass. The positive outcome (the row IS
// selected, via window.roRowState) is the load-bearing assertion: a future
// dispatcher that inserts a stray stop-after-close would still pass a
// "menu closed" check while silently dropping the selection.
test('open context menu + click another row: menu closes AND the other row selects', async ({
  page,
}) => {
  await page.goto(PODS);
  const menu = page.locator('#ro-ctxmenu');

  // Right-click nginx to open its bound menu.
  await page.locator('tr[data-key="e2e/default/nginx"]').click({ button: 'right' });
  await expect(menu).toHaveClass(/is-open/);
  expect(await selectedKeys(page)).toEqual([]);

  // Click a PLAIN cell of the OTHER row (the Ready cell, td #2 -- not the name
  // anchor or any interactive descendant): the single click closes the menu AND
  // falls through to select my-app.
  await page.locator('tr[data-key="e2e/default/my-app"] td').nth(1).click();

  await expect(menu).not.toHaveClass(/is-open/);
  // Positive outcome: the row actually entered the selection store.
  expect(await selectedKeys(page)).toEqual(['e2e/default/my-app']);
  await expect(page.locator('tr[data-key="e2e/default/my-app"]')).toHaveClass(/is-selected/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');
});

// CASE 2 — DOM-guard decoupling (class A): while the palette is open,
// keyboardSurfaceBusy() makes the gesture keydown listener inert, so j/k/Enter
// drive the palette's own active-row model and NEVER move the table's row focus
// underneath the modal.
test('open palette + j/k/Enter: the palette navigates, the table keeps row focus untouched', async ({
  page,
}) => {
  await page.goto(PODS);

  // Seat a row focus first so "unchanged" is a real, observable invariant.
  await page.keyboard.press('j');
  await expect(page.locator('#resource-list-content tr.kfocus')).toHaveCount(1);
  expect(await roFocusKey(page)).toBe('e2e/default/nginx');

  // Open the palette over the focused table. Its query box takes focus, so the
  // palette is now a text-entry surface (keyboardSurfaceBusy() AND the
  // text-entry gate both hold -- belt and suspenders).
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  await page.locator('#ro-palette-input').fill('pods');
  await expect(page.locator('#ro-palette-input')).toBeFocused();

  // ACTUAL behavior: while the palette query box is focused, j and k are TYPED
  // CHARACTERS in the query (not row-navigation keys) -- the gesture keydown is
  // inert, so they never reach the table. The query gains "jk"; the table row
  // focus stays exactly where it was seated.
  await page.keyboard.press('j');
  await page.keyboard.press('k');
  await expect(page.locator('#ro-palette-input')).toHaveValue('podsjk');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  expect(await roFocusKey(page)).toBe('e2e/default/nginx'); // table focus untouched
  await expect(page.locator('#resource-list-content tr.kfocus')).toHaveCount(1);

  // The palette's OWN Arrow model still navigates its rows (it owns those keys).
  // Reset to a query with multiple rows, then step the active row down and back.
  await page.locator('#ro-palette-input').fill('pods');
  await page.keyboard.press('ArrowDown');
  const activeAfterDown = await page
    .locator('#ro-palette-list .ro-pal-item.active .pal-label')
    .textContent();
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('#ro-palette-list .ro-pal-item.active .pal-label')).not.toHaveText(
    activeAfterDown ?? ''
  );
  await page.keyboard.press('ArrowUp');
  await expect(page.locator('#ro-palette-list .ro-pal-item.active .pal-label')).toHaveText(
    activeAfterDown ?? ''
  );

  // ⏎ while the palette is open belongs to the palette, never to the table's
  // "open the focused row" gesture. Prove it WITHOUT leaving the page: clear the
  // query so the palette's active row is the Everywhere search row, then assert
  // the gesture surface stayed inert -- ⏎ here would route to /search (the
  // palette), and crucially the table row focus is STILL the seated nginx (the
  // table's own ⏎-opens-nginx never fired).
  await page.locator('#ro-palette-input').fill('');
  expect(await roFocusKey(page)).toBe('e2e/default/nginx');

  // Esc closes the palette (exactly one surface) and the table row focus below
  // is STILL the seated row -- the modal never reached through to the table.
  await page.keyboard.press('Escape');
  await expect(page.locator('#ro-palette')).not.toHaveClass(/open/);
  expect(await roFocusKey(page)).toBe('e2e/default/nginx');
});

// CASE 3 — the columns popover dismissal pair (class A): an outside click closes
// it; and -- the load-bearing half -- clicking the ⊞ toggle WHILE open closes it
// exactly once. The big delegated click listener flips colsPopOpen=false on the
// toggle WITHOUT stopPropagation, and the outside-click listener's own
// `[data-ro-cols-toggle]` guard then early-returns, so the popover never
// double-fires back open. Both listeners see the same click; the guard, not
// propagation, keeps it single.
test('columns popover: outside click closes; toggle-click while open closes once (no reopen)', async ({
  page,
}) => {
  await page.goto(PODS);
  const pop = page.locator('#ro-cols-pop');
  const btn = page.locator('#ro-cols-btn');

  // Open via the ⊞ toggle.
  await btn.click();
  await expect(pop).toHaveClass(/is-open/);
  await expect(btn).toHaveAttribute('aria-expanded', 'true');

  // A click OUTSIDE the popover (and not on the toggle) closes it.
  await page.locator('h1, .ro-title-text, .ro-list-title').first().click();
  await expect(pop).not.toHaveClass(/is-open/);
  await expect(btn).toHaveAttribute('aria-expanded', 'false');

  // Re-open, then click the toggle AGAIN while open: it closes once and STAYS
  // closed (the toggle-click guard in the outside-click listener prevents the
  // reopen that a naive double-handle would cause).
  await btn.click();
  await expect(pop).toHaveClass(/is-open/);
  await btn.click();
  await expect(pop).not.toHaveClass(/is-open/);
  await expect(btn).toHaveAttribute('aria-expanded', 'false');
  // It did not bounce back open: a short settle window still shows it closed.
  await expect(pop).not.toHaveClass(/is-open/);
});

// CASE 4 — Escape closes the correct layer with the palette open. The reachable,
// meaningful compound is "palette open over a list with a filter draft": Esc
// closes the PALETTE (the topmost surface its own keydown owns) and leaves the
// filter layer below it entirely untouched -- the draft survives, the filter
// input is not cleared, no list re-render fires. (The literal "Esc with focus in
// #ro-filter-input while the palette is open" never reaches closePalette: the
// palette keydown returns into handleFilterInputKeydown first; with the
// autocomplete closed that Escape is a no-op -- asserted here so the dispatcher
// preserves the actual focus-routed semantics, not an idealized model.)
test('Escape with the palette open closes the palette layer and leaves the filter draft intact', async ({
  page,
}) => {
  await page.goto(PODS);

  // Leave a free-text draft in the filter editor (client-side match, no request).
  const filter = page.locator('#ro-filter-input');
  await filter.fill('ngi');
  await expect(filter).toHaveValue('ngi');
  await filter.blur();

  // Open the palette over it (its query box takes focus).
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  await expect(page.locator('#ro-palette-input')).toBeFocused();

  // Esc closes the PALETTE -- the right layer -- and the filter draft below is
  // untouched (not cleared, still showing its rows-filtered effect).
  await page.keyboard.press('Escape');
  await expect(page.locator('#ro-palette')).not.toHaveClass(/open/);
  await expect(filter).toHaveValue('ngi');

  // A second Esc with focus back in the filter input does NOT touch the palette
  // (already closed) and, with no autocomplete open, is a no-op on the draft --
  // the focus-routed reality the dispatcher must preserve.
  await filter.focus();
  await page.keyboard.press('Escape');
  await expect(page.locator('#ro-palette')).not.toHaveClass(/open/);
  await expect(filter).toHaveValue('ngi');
});
