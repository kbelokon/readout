import { test, expect, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { controlURL } from './playwright.config';

// Download surface: downloads must be REAL downloads.
//
//   - hx-boost regression (verified live before the fix): the body-level
//     boost captured a download anchor's click and swapped the raw attachment
//     bytes into <body> instead of downloading. The TSV list anchor and the
//     detail YAML anchor now carry hx-boost="false"; clicking them fires a
//     real `download` event and the page stays intact.
//   - Bulk Download YAML: one multi-document `---`-separated file via
//     location.assign on a Content-Disposition URL -- the page never
//     navigates, so the selection SURVIVES (a download is not a screen
//     change).
//   - The selection cap: above 100 selected the button disables
//     and ONE toast announces the refusal (the only sanctioned download
//     toast; there is deliberately NO "ready" toast). Dropping back under
//     the cap re-enables.

const PODS = '/clusters/e2e/namespaces/default/pods';
const POD = '/clusters/e2e/namespaces/default/pods/nginx';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

// selectRow toggles a row through the row-click gesture: the Ready cell
// (td #2) is plain text, so the click cannot hit the name anchor or any
// other interactive descendant (same helper as gestures.spec.ts).
async function selectRow(page: Page, key: string): Promise<void> {
  await page.locator(`tr[data-key="${key}"] td`).nth(1).click();
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the download buttons ride the table chrome (below 760px the card layer replaces it, D22)'
  );
  await control('/__control/reset');
});

test('TSV title button is a real download; the page body is NOT replaced (hx-boost opt-out)', async ({
  page,
}) => {
  await page.goto(PODS);
  const downloadEvent = page.waitForEvent('download');
  await page.locator('a[title="Download resource list as Tab-Separated-Values (TSV)"]').click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('clusters_e2e_namespaces_default_pods.tsv');

  // Under the boost bug the raw TSV bytes replaced <body>; the fixed anchor
  // leaves the page fully intact on the same URL.
  expect(page.url()).toContain(PODS);
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(page.locator('#resource-list-content table.ro-table')).toBeVisible();
});

test('detail YAML download button is a real download; the page stays', async ({ page }) => {
  await page.goto(POD);
  const downloadEvent = page.waitForEvent('download');
  await page.locator('.ro-detail-actions a[title="Download resource object as YAML"]').click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('clusters_e2e_namespaces_default_pods_nginx.yaml');

  expect(page.url()).toContain(POD);
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(page.locator('.ro-detail-actions')).toBeVisible();
});

test('bulk Download YAML serves one ---separated multi-doc file; selection survives the download', async ({
  page,
}) => {
  await page.goto(PODS);
  await selectRow(page, 'e2e/default/nginx');
  await selectRow(page, 'e2e/default/my-app');
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');
  await expect(page.locator('#ro-bulk-download')).toBeEnabled();

  const downloadEvent = page.waitForEvent('download');
  await page.locator('#ro-bulk-download').click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('e2e_default_pods_bulk.yaml');

  const body = readFileSync((await download.path())!, 'utf8');
  const docs = body.split(/^---$/m);
  expect(docs).toHaveLength(2);
  expect(docs[0]).toContain('name: nginx');
  expect(docs[1]).toContain('name: my-app');

  // location.assign on a Content-Disposition URL downloads WITHOUT leaving
  // the page: the selection (explicit intent) survives, and no toast appears
  // -- a plain GET has no detached "ready" moment.
  expect(page.url()).toContain(PODS);
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');
  await expect(page.locator('tr.is-selected')).toHaveCount(2);
  await expect(page.locator('.ro-toast')).toHaveCount(0);
});

test('over-cap selection disables bulk download with ONE refusal toast; under-cap re-enables', async ({
  page,
}) => {
  await page.goto(PODS);
  // Selection beyond the rendered rows rides the roRowState seam (selection
  // is identity-keyed client state; ghost keys exercise the cap without 101
  // fixture rows).
  await page.evaluate(() => {
    for (let i = 0; i < 101; i++) {
      (window as any).roRowState.setSelected(`e2e/default/ghost-${i}`, true);
    }
  });
  // Growing the over-cap selection does NOT stack more toasts (one per
  // crossing). The growth happens IMMEDIATELY after the crossing -- before
  // any assertions -- so the whole no-stacking check runs well inside the
  // first toast's 3.5s lifetime (asserting on the 101 state first used to
  // eat into it).
  await page.evaluate(() => (window as any).roRowState.setSelected('e2e/default/ghost-101', true));
  await expect(page.locator('#ro-bulk-count')).toHaveText('102 selected');
  await expect(page.locator('#ro-bulk-download')).toBeDisabled();
  // Toast creation is synchronous inside the setSelected evaluates above, so
  // the count is asserted WITHOUT retry: a stacked second toast must fail
  // here, not be waited away.
  expect(await page.locator('.ro-toast').count()).toBe(1);
  // The text pins the toast to the 101 CROSSING (the 102 growth toasts nothing).
  await expect(page.locator('.ro-toast')).toHaveText('Download refused: 101 selected (max 100)');

  // The toast is transient: 3.5s visible, then it leaves on its own.
  await expect(page.locator('.ro-toast')).toHaveCount(0, { timeout: 6_000 });

  // Dropping back under the cap re-enables the download.
  await page.evaluate(() => {
    (window as any).roRowState.setSelected('e2e/default/ghost-100', false);
    (window as any).roRowState.setSelected('e2e/default/ghost-101', false);
  });
  await expect(page.locator('#ro-bulk-count')).toHaveText('100 selected');
  await expect(page.locator('#ro-bulk-download')).toBeEnabled();
});
