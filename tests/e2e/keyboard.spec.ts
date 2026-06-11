import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Keyboard navigation + the "?" overlay + the SPEC §8.6 accessibility floor
// (Unit 18 / D10 / D23), end to end:
//
//   - j/k move the row focus through the visible identity rows (kfocus class),
//     keyed by data-key through window.roRowState -- NOT by position -- and the
//     table wrap mirrors the focused row id as aria-activedescendant;
//   - ⏎ opens the focused row's detail (the same server-resolved data-href the
//     context menu's Open uses);
//   - the gesture keys are INERT while a text-entry surface or overlay owns
//     the keyboard (the chips editor's ⏎-commits-a-chip protocol above all,
//     and the ⌘K palette);
//   - focus survives a refresh-tick morph (the identity proof, like the
//     Unit 16 selection one);
//   - "?" toggles the keyboard-map overlay; esc/backdrop close it and restore
//     the prior focus; keys are trapped while it is open;
//   - the sorted header exposes aria-sort with the real direction.
//
// Driven against the fakeapi harness, same as gestures.spec.ts.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

async function scriptEvents(events: object[]): Promise<void> {
  const res = await fetch(`${controlURL}/__control/watch-script`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ events }),
  });
  if (!res.ok) {
    throw new Error(`watch-script: ${res.status} ${await res.text()}`);
  }
}

function podObject(name: string, created: string) {
  return {
    apiVersion: 'v1',
    kind: 'Pod',
    metadata: { name, namespace: 'default', creationTimestamp: created, uid: `uid-${name}` },
    status: { phase: 'Running' },
  };
}

// seedThird adds a third pod so j/j/k walks distinguishable rows and the
// reorder scenario has a row whose POSITION shifts under a focused key.
async function seedThird(): Promise<void> {
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: podObject('zeta', '2025-01-01T00:00:00Z'),
      cells: ['zeta', '1/1', 'Running', '0', '1y'],
    },
  ]);
}

function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function isUserTableResponse(r: Response): boolean {
  const headers = r.request().headers();
  return (
    r.url().includes('/_table') &&
    headers['ro-no-push'] !== 'true' &&
    headers['hx-preloaded'] !== 'true'
  );
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

async function clickSort(page: Page, label: string): Promise<void> {
  const swapped = page.waitForResponse(isUserTableResponse);
  await page.locator('thead th a', { hasText: label }).first().click();
  await swapped;
}

// The navbar interval menu opens on hover (CSS :hover/:focus-within).
async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-ro-interval="${secs}"]`).click();
}

const focusedRow = '#resource-list-content tr.kfocus';

// expectFocus asserts the SINGLE focused row is `key` and that the table wrap
// announces exactly that row via aria-activedescendant (SPEC §8.6).
async function expectFocus(page: Page, key: string): Promise<void> {
  await expect(page.locator(focusedRow)).toHaveCount(1);
  await expect(page.locator(`tr[data-key="${key}"]`)).toHaveClass(/kfocus/);
  const rowId = await page.locator(`tr[data-key="${key}"]`).getAttribute('id');
  expect(rowId).toBeTruthy();
  await expect(page.locator('#resource-list-content .ro-table-wrap')).toHaveAttribute(
    'aria-activedescendant',
    rowId!
  );
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'keyboard row navigation is a desktop surface (below 760px the card layer replaces the table, D22)'
  );
  await control('/__control/reset');
});

test('j/j/k moves the row focus by data-key; the wrap mirrors it as aria-activedescendant', async ({
  page,
}) => {
  await seedThird();
  await page.goto(PODS);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'nginx',
    'my-app',
    'zeta',
  ]);

  // No focus until the first j.
  await expect(page.locator(focusedRow)).toHaveCount(0);

  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/nginx');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/my-app');
  await page.keyboard.press('k');
  await expectFocus(page, 'e2e/default/nginx');

  // k at the top clamps (stays on the first row).
  await page.keyboard.press('k');
  await expectFocus(page, 'e2e/default/nginx');
});

test('gesture keys stay inert while the filter editor or the palette owns the keyboard', async ({
  page,
}) => {
  await page.goto(PODS);
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/nginx');

  // The chips editor input keeps its own keymap: j and ? are CHARACTERS here.
  await page.locator('#ro-filter-input').click();
  await page.keyboard.press('j');
  await page.keyboard.press('?');
  await expect(page.locator('#ro-filter-input')).toHaveValue('j?');
  await expect(page.locator('#ro-kbd-overlay')).not.toHaveClass(/open/);
  await expectFocus(page, 'e2e/default/nginx'); // unmoved

  // Clear the draft, blur the editor, open the ⌘K palette: j filters the
  // palette, it never moves the row focus underneath the modal.
  await page.locator('#ro-filter-input').fill('');
  await page.locator('#ro-filter-input').blur();
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  await page.keyboard.press('j');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  await expectFocus(page, 'e2e/default/nginx'); // still unmoved
  await page.keyboard.press('Escape');
  await expect(page.locator('#ro-palette')).not.toHaveClass(/open/);
});

test('⏎ opens the focused row detail', async ({ page }) => {
  await page.goto(PODS);
  await page.keyboard.press('j');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/my-app');

  await page.keyboard.press('Enter');
  await page.waitForURL(`**${PODS}/my-app`);
  await expect(page.locator('.ro-topbar')).toBeVisible();
});

test('row focus survives a refresh-tick morph that reorders rows (keyed, not positional)', async ({
  page,
}) => {
  await seedThird();
  await page.goto(PODS);

  // Sort by Name (my-app, nginx, zeta) and focus zeta -- the LAST row.
  await clickSort(page, 'Name');
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'my-app',
    'nginx',
    'zeta',
  ]);
  await page.keyboard.press('j');
  await page.keyboard.press('j');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/zeta');

  // A tick lands omega, which sorts BETWEEN nginx and zeta: zeta's position
  // shifts from 2 to 3. The focus must follow the key, never the position.
  await pickInterval(page, 5);
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: podObject('omega', '2025-06-01T00:00:00Z'),
      cells: ['omega', '1/1', 'Running', '0', '1y'],
    },
  ]);
  await waitForTick(page);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'my-app',
    'nginx',
    'omega',
    'zeta',
  ]);
  await expectFocus(page, 'e2e/default/zeta');
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).not.toHaveClass(/kfocus/);
});

test('deleting the focused row clears aria-activedescendant and kfocus; j clamps at the bottom', async ({
  page,
}) => {
  await seedThird();
  await page.goto(PODS);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'nginx',
    'my-app',
    'zeta',
  ]);

  // Walk to the BOTTOM row; one more j clamps there (the j twin of the
  // existing k-at-the-top clamp).
  await page.keyboard.press('j');
  await page.keyboard.press('j');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/zeta');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/zeta');

  // Delete the focused row out from under its key: the next tick's morph
  // drops the row, and the row-state re-apply must CLEAR the wrap's
  // aria-activedescendant (SPEC §8.6: never announce a row that left the
  // document) with zero kfocus rows -- the clear branch of the focus mirror.
  await pickInterval(page, 5);
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'DELETED',
      object: { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'zeta', namespace: 'default' } },
    },
  ]);
  await waitForTick(page);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'nginx',
    'my-app',
  ]);
  await expect(page.locator(focusedRow)).toHaveCount(0);
  await expect(page.locator('#resource-list-content .ro-table-wrap')).not.toHaveAttribute(
    'aria-activedescendant'
  );
});

test('j/k walk only the visible rows: live-filtered rows are skipped', async ({ page }) => {
  await seedThird();
  await page.goto(PODS);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'nginx',
    'my-app',
    'zeta',
  ]);

  // A live free-text draft ("a") hides nginx (no request -- the chips
  // editor's client-side name match toggles ro-row-filtered) and keeps
  // my-app + zeta visible. Blur the editor so the gesture keys re-arm.
  await page.locator('#ro-filter-input').fill('a');
  await page.locator('#ro-filter-input').blur();
  await expect(page.locator('tr[data-key="e2e/default/nginx"]')).toHaveClass(/ro-row-filtered/);
  await expect(page.locator('tr[data-key="e2e/default/my-app"]')).not.toHaveClass(
    /ro-row-filtered/
  );
  await expect(page.locator('tr[data-key="e2e/default/zeta"]')).not.toHaveClass(/ro-row-filtered/);

  // The first j lands on the first VISIBLE row (my-app) -- the hidden nginx
  // is skipped, not focused-then-stepped-over.
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/my-app');
  await page.keyboard.press('j');
  await expectFocus(page, 'e2e/default/zeta');
  // k walks back up and clamps on the visible top: never onto hidden nginx.
  await page.keyboard.press('k');
  await expectFocus(page, 'e2e/default/my-app');
  await page.keyboard.press('k');
  await expectFocus(page, 'e2e/default/my-app');
  await expect(page.locator('tr[data-key="e2e/default/nginx"]')).not.toHaveClass(/kfocus/);
});

test('"?" opens the keyboard overlay focus-trapped; esc closes and returns focus; backdrop closes', async ({
  page,
}) => {
  await page.goto(PODS);
  const overlay = page.locator('#ro-kbd-overlay');
  const card = page.locator('#ro-kbd-overlay .kbd-card');

  // Seat keyboard focus somewhere identifiable first.
  await page.locator('#btn-theme-toggle').focus();
  await page.keyboard.press('?');
  await expect(overlay).toHaveClass(/open/);
  await expect(overlay).toHaveAttribute('aria-hidden', 'false');
  await expect(card).toBeFocused();
  // Navigate / Act columns per the prototype card.
  await expect(page.locator('#ro-kbd-overlay .kbd-grp')).toHaveText(['Navigate', 'Act']);

  // The overlay is modal: j moves nothing behind it, Tab stays trapped on the
  // card (its only focus stop).
  await page.keyboard.press('j');
  await expect(page.locator(focusedRow)).toHaveCount(0);
  await page.keyboard.press('Tab');
  await expect(card).toBeFocused();

  // esc closes and returns focus to the opener.
  await page.keyboard.press('Escape');
  await expect(overlay).not.toHaveClass(/open/);
  await expect(page.locator('#btn-theme-toggle')).toBeFocused();

  // "?" toggles: open again, "?" closes.
  await page.keyboard.press('?');
  await expect(overlay).toHaveClass(/open/);
  await page.keyboard.press('?');
  await expect(overlay).not.toHaveClass(/open/);

  // Backdrop click (outside the centered card) closes too.
  await page.keyboard.press('?');
  await expect(overlay).toHaveClass(/open/);
  await overlay.click({ position: { x: 10, y: 10 } });
  await expect(overlay).not.toHaveClass(/open/);
});

test('the sorted header exposes aria-sort with the live direction', async ({ page }) => {
  await page.goto(`${PODS}?sort=Name`);

  // Exactly ONE header carries aria-sort, and it is the ascending Name column.
  await expect(page.locator('thead th[aria-sort]')).toHaveCount(1);
  await expect(page.locator('thead th', { hasText: 'Name' })).toHaveAttribute(
    'aria-sort',
    'ascending'
  );

  // Toggling the sort flips the announced direction with the visual one.
  await clickSort(page, 'Name');
  await expect(page.locator('thead th', { hasText: 'Name' })).toHaveAttribute(
    'aria-sort',
    'descending'
  );
  await expect(page.locator('thead th[aria-sort]')).toHaveCount(1);
});
