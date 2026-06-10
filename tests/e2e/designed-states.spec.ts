import { test, expect } from '@playwright/test';
import { controlURL } from './playwright.config';

// The designed states (SPEC §7.18 / D16) against the live binary:
//
//   - fail-lists?mode=403 renders the forbidden card with the VERBATIM
//     apiserver Status message in the mono errdetail block (the verbatim-error
//     law, SPEC §1.5) under one plain-language headline;
//   - fail-lists?mode=500 renders the unreachable card with the VERBATIM
//     InternalError Status message;
//   - a filter that hides every row renders the empty-FILTERED card with the
//     active chips inline + Clear filters;
//   - a cluster-scoped kind (nodes) shows no namespace breadcrumb segment and
//     no "across all namespaces" link (SPEC §5/§9);
//   - an auto-refresh failure dims the last-good rows and reveals the warn
//     banner ("Auto-refresh failed — showing the last good data" + Retry now)
//     WITHOUT blanking the table (the data-never-disappears law).

const PODS = '/clusters/e2e/namespaces/default/pods';

// The exact Status messages the fakeapi control surface emits (control.go):
// the cards must carry them verbatim.
const FORBIDDEN_MESSAGE =
  'pods is forbidden: User "viewer" cannot list resource "pods" in API group "" in the namespace "default"';
const INTERNAL_MESSAGE = 'Internal error occurred: fakeapi fail-lists mode 500 is active';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the designed-states walk asserts the desktop table/breadcrumb chrome'
  );
  await control('/__control/reset');
});

test('mode=403 renders the forbidden card with the verbatim Status message', async ({ page }) => {
  await control('/__control/fail-lists?mode=403');
  await page.goto(PODS);

  const card = page.locator('.ro-empty-lg');
  await expect(card.locator('h3')).toHaveText('Not allowed to list pods in “default”');
  // Warn lock tile + the one plain-language line above the verbatim block.
  await expect(card.locator('.ro-empty-glyph.warn')).toBeVisible();
  await expect(card.locator('p')).toHaveText(
    'Your credentials can browse this cluster, but RBAC denies this view.'
  );
  // The REAL apiserver string, verbatim, in the mono errdetail block.
  await expect(card.locator('.errdetail')).toContainText('403 Forbidden');
  await expect(card.locator('.errdetail')).toContainText(FORBIDDEN_MESSAGE);
  // Retry + Back to clusters, both plain anchors (read-only GETs).
  await expect(card.locator('.ro-empty-actions a', { hasText: 'Retry' })).toBeVisible();
  await expect(card.locator('.ro-empty-actions a', { hasText: 'Back to clusters' })).toHaveAttribute(
    'href',
    '/clusters'
  );
  // The state replaces the table entirely.
  await expect(page.locator('#resource-list-content table.ro-table')).toHaveCount(0);
});

test('mode=500 renders the unreachable card with the verbatim error string', async ({ page }) => {
  await control('/__control/fail-lists?mode=500');
  await page.goto(PODS);

  const card = page.locator('.ro-empty-lg');
  await expect(card.locator('h3')).toHaveText('Can’t reach e2e');
  await expect(card.locator('.ro-empty-glyph.err')).toBeVisible();
  await expect(card.locator('p')).toHaveText('The apiserver answered with an error.');
  await expect(card.locator('.errdetail')).toContainText(INTERNAL_MESSAGE);
  await expect(card.locator('.ro-empty-actions a', { hasText: 'Retry' })).toBeVisible();

  // Recovery: untoggle and Retry (the read-only GET) restores the table.
  await control('/__control/fail-lists?mode=off');
  await page.locator('.ro-empty-actions a', { hasText: 'Retry' }).click();
  await expect(page.locator('#resource-list-content table.ro-table tbody tr')).not.toHaveCount(0);
});

test('empty-filtered shows the active chips inline plus Clear filters', async ({ page }) => {
  await page.goto(`${PODS}?filter=zzz-no-such-pod`);

  const card = page.locator('.ro-empty-row .ro-empty-lg');
  await expect(card.locator('h3')).toHaveText('No Pods match the active filters');
  // The active chip rides INSIDE the card; its ✕ is a read-only GET.
  const chip = card.locator('.ro-empty-chips .ro-scope-chip');
  await expect(chip).toHaveCount(1);
  await expect(chip).toContainText('zzz-no-such-pod');
  await expect(chip.locator('a.chip-x')).toHaveAttribute('href', PODS);
  // Clear filters drops the set and lands back on the populated list.
  await card.locator('.ro-empty-actions a', { hasText: 'Clear filters' }).click();
  await expect(page.locator('#resource-list-content table.ro-table tbody td.cell-name').first()).toBeVisible();
});

test('a cluster-scoped kind shows no namespace crumb and no all-namespaces link', async ({ page }) => {
  await page.goto('/clusters/e2e/nodes');

  // Breadcrumb: exactly cluster + plural -- no namespace segment (SPEC §5/§9).
  const crumbs = page.locator('.ro-rd .ro-breadcrumb li');
  await expect(crumbs).toHaveText(['e2e', 'nodes']);
  // No "Show nodes across all namespaces" footer link.
  await expect(page.locator('.ro-table-meta a', { hasText: 'across all namespaces' })).toHaveCount(0);

  // The namespaced control: pods in a namespace DOES offer both.
  await page.goto(PODS);
  await expect(page.locator('.ro-rd .ro-breadcrumb li')).toHaveText(['e2e', 'default', 'pods']);
  await expect(page.locator('.ro-table-meta a', { hasText: 'across all namespaces' })).toBeVisible();
});

test('a failed auto-refresh dims the last-good rows and reveals the stale banner', async ({ page }) => {
  await page.goto(PODS);
  const rows = page.locator('#resource-list-content table.ro-table tbody td.cell-name');
  await expect(rows).toHaveText(['nginx', 'my-app']);

  // Arm a 5s refresh, then break every list: the next tick errors (non-2xx).
  await page.locator('#refresh-dropdown').hover();
  await page.locator('.refresh-option[data-interval="5"]').click();
  // Park the cursor mid-page: the hover-opened refresh menu would otherwise
  // stay open over the banner and intercept the Retry-now click below.
  await page.mouse.move(200, 400);
  await control('/__control/fail-lists?mode=500');

  // The banner reveals with the D16 copy; the rows DIM but never disappear.
  const banner = page.locator('.ro-stale-banner');
  await expect(banner).toBeVisible({ timeout: 15_000 });
  await expect(banner.locator('.bn-title')).toHaveText(
    'Auto-refresh failed — showing the last good data'
  );
  await expect(banner.locator('.ro-stale-retry')).toHaveText('Retry now');
  await expect(rows).toHaveText(['nginx', 'my-app']);
  await expect(page.locator('#resource-list-content')).toHaveClass(/ro-stale/);

  // Recovery: fix the backend, Retry now -> banner hides, dim clears.
  await control('/__control/fail-lists?mode=off');
  await banner.locator('.ro-stale-retry').click();
  await expect(banner).toBeHidden({ timeout: 15_000 });
  await expect(page.locator('#resource-list-content')).not.toHaveClass(/ro-stale/);
  await expect(rows).toHaveText(['nginx', 'my-app']);
});
