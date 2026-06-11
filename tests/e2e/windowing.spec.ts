import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Virtualization (Unit 24 / D20 / SPEC §8.2) on the 600-row "big" fixtures —
// ordinary fakeapi store state, no injection endpoint:
//
//   - above ~500 rows the client windows the tbody: the DOM holds far fewer
//     than 600 <tr> AND far fewer than 600 .ro-pcard (the mobile card
//     projection is suppressed server-side), while the footer states the TRUE
//     total — no pagination affordance anywhere;
//   - scrolling reaches the LAST row by name (the spacer offset math holds
//     end to end);
//   - j/k crosses a window boundary: the walker runs on the full visible list
//     and the focus jump scrolls the window;
//   - identity-keyed selection survives a sort swap while windowed;
//   - an awaited tick while windowed leaves the window intact — no duplicate
//     rows, stable scroll — and adopted changes flash exactly like the
//     idiomorph path (the review-proven pass-with-bug hole #1);
//   - the 600-row EVENTS fixture renders one-line clamped messages (full text
//     in the title — the D20 fixed-height law), while the below-threshold
//     events list keeps the full SPEC wrap;
//   - free text matches a name currently OUTSIDE the rendered window (the
//     matcher reads the full row model, never the DOM window — the
//     review-proven pass-with-bug hole #2).

const BIG_PODS = '/clusters/e2e/namespaces/big/pods';
const BIG_EVENTS = '/clusters/e2e/namespaces/big/events';
const BIG_PODS_LIST_PATH = '/api/v1/namespaces/big/pods';

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

// A tick marks itself RO-No-Push (the refresh.spec.ts pattern).
function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-interval="${secs}"]`).click();
  await page.mouse.move(200, 400); // park the cursor: close the hover menu
}

function identityRows(page: Page) {
  return page.locator('#resource-list-content tbody tr[data-key]');
}

function podKey(n: number): string {
  return `e2e/big/big-pod-${String(n).padStart(4, '0')}`;
}

function podRow(page: Page, n: number) {
  return page.locator(`tr[data-key="${podKey(n)}"]`);
}

test.describe('windowing (desktop)', () => {
  test.beforeEach(async ({}, testInfo) => {
    test.skip(
      testInfo.project.name !== 'desktop',
      'the windowing gestures (j/k, sort headers, refresh chrome) are desktop surfaces; the mobile half has its own test below'
    );
    await control('/__control/reset');
  });

  test('engages above the threshold: windowed rows, suppressed cards, true total', async ({
    page,
  }) => {
    await page.goto(BIG_PODS);

    // Far fewer than 600 <tr> — the window plus its buffer.
    await expect(podRow(page, 1)).toBeVisible();
    const rows = await identityRows(page).count();
    expect(rows).toBeGreaterThan(10);
    expect(rows).toBeLessThan(100);

    // Card suppression: ZERO .ro-pcard in the DOM (600 hidden card subtrees
    // must not ride every morph), not merely hidden ones.
    await expect(page.locator('.ro-pcard')).toHaveCount(0);
    await expect(page.locator('.ro-cardlist')).toHaveCount(0);

    // The windowing furniture: the server marker, the two spacer rows, the
    // pinned fixed layout.
    await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
    await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
    await expect(page.locator('table.ro-table.ro-virtualized')).toHaveCount(1);

    // The footer and the count chip state the TRUE total — and no pagination
    // affordance exists anywhere.
    await expect(page.locator('.ro-count')).toHaveText('600');
    await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');
  });

  test('scrolling reaches the last row by name', async ({ page }) => {
    await page.goto(BIG_PODS);
    await expect(podRow(page, 1)).toBeVisible();
    await expect(podRow(page, 600)).toHaveCount(0); // far outside the window

    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect(podRow(page, 600)).toBeVisible();
    // ... and the window MOVED (the top rows left the DOM): this is a window,
    // not a grow-only render.
    await expect(podRow(page, 1)).toHaveCount(0);
    const rows = await identityRows(page).count();
    expect(rows).toBeLessThan(100);
  });

  test('j/k crosses a window boundary: the focus jump scrolls the window', async ({ page }) => {
    await page.goto(BIG_PODS);
    await expect(podRow(page, 1)).toBeVisible();

    // The initial render stops at the window edge; walk j past it.
    const bounds = await page.evaluate(() => (window as any).roVirtual.renderedBounds());
    expect(bounds.total).toBe(600);
    expect(bounds.end).toBeLessThan(100);
    const presses = bounds.end + 5; // strictly beyond the initial window

    for (let i = 0; i < presses; i++) {
      await page.keyboard.press('j');
    }
    // The first j lands on row 1, so N presses focus row N — a row that was
    // NOT in the initial window; the jump scrolled the window to it.
    const focused = page.locator('tr.kfocus');
    await expect(focused).toHaveCount(1);
    await expect(focused).toHaveAttribute('data-key', podKey(presses));
    await expect(focused).toBeInViewport();

    // k walks back across the same boundary.
    await page.keyboard.press('k');
    await expect(page.locator('tr.kfocus')).toHaveAttribute('data-key', podKey(presses - 1));
  });

  test('identity-keyed selection survives a sort while windowed', async ({ page }) => {
    await page.goto(BIG_PODS);
    const row2 = podRow(page, 2);
    await expect(row2).toBeVisible();

    // Row-click selection on a non-interactive cell (the Ready cell).
    await row2.locator('td').nth(1).click();
    await expect(row2).toHaveClass(/is-selected/);
    await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');

    // Sort by Status: the fixture mixes Running/Pending, so big-pod-0002
    // (Running) moves deep into the dataset — outside the initial window.
    const sorted = page.waitForResponse(
      (r) => r.url().includes('/_table') && r.url().includes('sort='),
      { timeout: 15_000 }
    );
    await page
      .locator('#resource-list-content thead th a')
      .filter({ hasText: /^Status/ })
      .click();
    await sorted;

    // The store survives the swap; the selected row actually LEFT the window
    // (its <tr> is detached — Status sort puts the ~85-row Pending block
    // first, pushing big-pod-0002 to ~index 85, far past the rendered
    // window). Without this the scrollToKey jump below could be a no-op on a
    // still-rendered row, never proving re-decoration after a re-window; it
    // doubles as the swap-completion barrier (the bulk-count text is
    // identical before and after the swap, so it cannot be one).
    await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');
    await expect(row2).toHaveCount(0);
    const jumped = await page.evaluate(
      (key) => (window as any).roVirtual.scrollToKey(key),
      podKey(2)
    );
    expect(jumped).toBe(true);
    await expect(row2).toHaveClass(/is-selected/);
    // Still windowed after the sort swap (every morph source re-windows).
    expect(await identityRows(page).count()).toBeLessThan(100);
  });

  test('an awaited tick while windowed leaves the window intact', async ({ page }) => {
    await page.goto(BIG_PODS);
    await expect(podRow(page, 1)).toBeVisible();
    await pickInterval(page, 5);

    // Mutate a row through the fixture LIST state, then park mid-list: the
    // tick must keep the window and the scroll position exactly.
    await scriptEvents([
      {
        path: BIG_PODS_LIST_PATH,
        type: 'MODIFIED',
        object: { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'big-pod-0001', namespace: 'big' } },
        cells: ['big-pod-0001', '0/1', 'CrashLoopBackOff', '3', '10m'],
      },
    ]);
    await page.evaluate(() => window.scrollTo(0, 4000));
    await expect
      .poll(() => page.evaluate(() => window.scrollY), { timeout: 5_000 })
      .toBeGreaterThan(3500);
    // The re-window rides a rAF-throttled scroll listener: wait until the
    // rendered slice actually moved (the live.spec.ts pattern), then mutate a
    // row INSIDE the scrolled-to window too — its new text is the adoption
    // barrier below (waitForTick resolves on the HTTP response, which races
    // the swap + adoption the asserts depend on).
    await expect
      .poll(
        () =>
          page.evaluate(
            () =>
              (window as unknown as { roVirtual: { renderedBounds(): { start: number } } })
                .roVirtual.renderedBounds().start
          ),
        { timeout: 5_000 }
      )
      .toBeGreaterThan(50);
    const bounds = await page.evaluate(
      () =>
        (window as unknown as { roVirtual: { renderedBounds(): { start: number; end: number } } })
          .roVirtual.renderedBounds()
    );
    const inWindowPod = bounds.start + 7; // visible list index i is big-pod-(i+1)
    await scriptEvents([
      {
        path: BIG_PODS_LIST_PATH,
        type: 'MODIFIED',
        object: {
          apiVersion: 'v1',
          kind: 'Pod',
          metadata: { name: `big-pod-${String(inWindowPod).padStart(4, '0')}`, namespace: 'big' },
        },
        cells: [`big-pod-${String(inWindowPod).padStart(4, '0')}`, '0/1', 'ImagePullBackOff', '1', '10m'],
      },
    ]);
    const scrollBefore = await page.evaluate(() => window.scrollY);

    await waitForTick(page);
    // Adoption barrier: the in-window row shows the mutated text, so the
    // swap AND the virtualizer's adoption have fully landed — the stable-
    // scroll and no-dupes asserts below run against the post-adoption DOM,
    // never the pre-swap one. The generous timeout covers a tick that was
    // already in flight when the mutation posted (the NEXT tick carries it).
    await expect(podRow(page, inWindowPod).locator('td').nth(2)).toContainText(
      'ImagePullBackOff',
      { timeout: 15_000 }
    );

    // Stable scroll, still windowed, NO duplicate rows.
    expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);
    const keys = await page.evaluate(() =>
      Array.from(
        document.querySelectorAll('#resource-list-content tbody tr[data-key]'),
        (tr) => tr.getAttribute('data-key')
      )
    );
    expect(keys.length).toBeGreaterThan(10);
    expect(keys.length).toBeLessThan(100);
    expect(new Set(keys).size).toBe(keys.length);
    await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
    await expect(podRow(page, 1)).toHaveCount(0); // row 1 stays out-of-window

    // The adoption carried the new content: back at the top the mutated row
    // shows the new status.
    await page.evaluate(() => window.scrollTo(0, 0));
    await expect(podRow(page, 1)).toBeVisible();
    await expect(podRow(page, 1).locator('td').nth(2)).toContainText('CrashLoopBackOff');

    // ... and a change adopted while its row IS rendered flashes the changed
    // cell (the §8.3 net: windowed rows bypass idiomorph, the virtualizer
    // diffs them itself).
    await scriptEvents([
      {
        path: BIG_PODS_LIST_PATH,
        type: 'MODIFIED',
        object: { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'big-pod-0002', namespace: 'big' } },
        cells: ['big-pod-0002', '0/1', 'Error', '5', '10m'],
      },
    ]);
    await waitForTick(page);
    await expect(podRow(page, 2).locator('td').nth(2)).toContainText('Error');
    await expect(podRow(page, 2).locator('td').nth(2)).toHaveClass(/ro-cell-changed/);
    await expect(podRow(page, 2).locator('td.cell-name')).not.toHaveClass(/ro-cell-changed/);
  });

  test('the 600-row events list clamps messages to one line while windowed', async ({ page }) => {
    await page.goto(BIG_EVENTS);
    const rows = identityRows(page);
    await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
    expect(await rows.count()).toBeLessThan(100);
    await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');

    // One-line clamp with the FULL text recoverable in the title (D20: the
    // recorded fixed-height deviation).
    const msg = page.locator('td.ro-event-msg').first();
    await expect(msg).toHaveCSS('white-space', 'nowrap');
    const { title, text } = await msg.evaluate((td) => ({
      title: td.getAttribute('title'),
      text: (td.textContent || '').trim(),
    }));
    expect(title).toBe(text);
    expect(text.length).toBeGreaterThan(120); // the fixture messages would wrap unclamped

    // The fixed-height law holds across the rendered window: every row the
    // window shows has the SAME height.
    const heights = await page.evaluate(() =>
      Array.from(
        document.querySelectorAll('#resource-list-content tbody tr[data-key]'),
        (tr) => tr.getBoundingClientRect().height
      )
    );
    expect(Math.max(...heights) - Math.min(...heights)).toBeLessThan(1);

    // Below the threshold the full SPEC behavior is untouched: the default
    // namespace events wrap and carry no clamp title.
    await page.goto('/clusters/e2e/namespaces/default/events');
    const smallMsg = page.locator('td.ro-event-msg').first();
    await expect(smallMsg).toHaveCSS('white-space', 'normal');
    await expect(smallMsg).not.toHaveAttribute('title');
  });

  test('free text matches a name currently OUTSIDE the rendered window', async ({ page }) => {
    await page.goto(BIG_PODS);
    await expect(podRow(page, 1)).toBeVisible();
    await expect(podRow(page, 550)).toHaveCount(0); // the target is NOT rendered

    // The live matcher runs on the FULL row model (never the DOM window): the
    // out-of-window name narrows to its row.
    await page.locator('#ro-filter-input').fill('0550');
    await expect(podRow(page, 550)).toBeVisible();
    await expect(identityRows(page)).toHaveCount(1);
    // The footer keeps stating the SERVER truth (the live match is a lens,
    // not a filter request).
    await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');

    // Clearing the draft restores the window.
    await page.locator('#ro-filter-input').fill('');
    await expect
      .poll(() => identityRows(page).count(), { timeout: 5_000 })
      .toBeGreaterThan(10);
    expect(await identityRows(page).count()).toBeLessThan(100);
  });
});

test.describe('windowing (mobile)', () => {
  test.beforeEach(async ({}, testInfo) => {
    test.skip(
      testInfo.project.name !== 'mobile',
      'the card-suppression fallback below 760px is the mobile half of this matrix'
    );
    await control('/__control/reset');
  });

  test('a big list keeps the (windowed) table below 760px: cards are suppressed', async ({
    page,
  }) => {
    await page.goto(BIG_PODS);
    // No card layer at all — and the wrap is NOT hidden (it dropped
    // `has-cards`), so the page is a horizontally-scrolling windowed table
    // instead of a blank screen.
    await expect(page.locator('.ro-pcard')).toHaveCount(0);
    await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
    await expect(podRow(page, 1)).toBeVisible();
    const rows = await identityRows(page).count();
    expect(rows).toBeGreaterThan(10);
    expect(rows).toBeLessThan(100);
  });
});
