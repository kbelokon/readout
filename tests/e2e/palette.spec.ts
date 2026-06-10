import { test, expect, type Page } from '@playwright/test';
import { controlURL } from './playwright.config';

// ⌘K palette v2 (Unit 19 / D21 + D12, SPEC §6.3 + §8.7), end to end:
//
//   - while TYPING the Everywhere row (`Search all clusters for "q"`) is
//     pinned first and ⏎ lands on /search?q=… (D12 routing);
//   - group order with a query: Everywhere -> On this page -> Resource types
//     -> Namespaces -> Clusters -> Actions; empty query: Recents first, then
//     the standard groups minus Everywhere;
//   - matching is the fuzzy SUBSEQUENCE ranker (window.roFuzzy -- a pure
//     function unit-tested in isolation through that seam): prefix beats
//     word-start beats scattered, and within a tier tighter/earlier wins
//     ("dply" lands Deployments at the top of its group);
//   - Recents: the last 5 chosen entries persist in localStorage
//     ('ro-pref-recents'), deduped by destination, newest first, recorded on
//     every palette navigation, shown on the empty query only;
//   - the v1 Actions-group content SURVIVES the rebuild: detail-tab jumps
//     (Default view / YAML / Events), the Logs jump, and Toggle theme -- the
//     review-flagged silent-regression path this spec pins;
//   - resource-type rows carry the kind icon, the API-group meta, and the
//     quiet namespaced/cluster scope badge (Unit 3 vocabulary, D3).
//
// Driven against the fakeapi harness, same as keyboard.spec.ts.

const PODS = '/clusters/e2e/namespaces/default/pods';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

async function openPalette(page: Page): Promise<void> {
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
}

// paletteGroups snapshots the rendered list as [{ title, labels[] }] in DOM
// order. Labels are read from the dataset (the full identity the recents
// recorder uses), falling back to the visible .pal-label text.
function paletteGroups(page: Page): Promise<{ title: string; labels: string[] }[]> {
  return page.evaluate(() => {
    const out: { title: string; labels: string[] }[] = [];
    document.querySelectorAll('#ro-palette-list > *').forEach((el) => {
      if (el.classList.contains('ro-pal-group')) {
        out.push({ title: el.textContent || '', labels: [] });
      } else if (el.classList.contains('ro-pal-item') && out.length > 0) {
        const label =
          (el as HTMLElement).dataset.label || el.querySelector('.pal-label')?.textContent || '';
        out[out.length - 1].labels.push(label);
      }
    });
    return out;
  });
}

const activeLabel = '#ro-palette-list .ro-pal-item.active .pal-label';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the palette typing flows are a desktop keyboard surface (the chord + arrow protocol)'
  );
  await control('/__control/reset');
});

test('typing pins Everywhere first and ⏎ lands on /search?q=…', async ({ page }) => {
  await page.goto(PODS);
  await openPalette(page);
  await page.locator('#ro-palette-input').fill('dply');

  // The Everywhere group leads while a query exists, its row is the seated
  // active row, and the label carries the live query verbatim.
  const groups = await paletteGroups(page);
  expect(groups[0].title).toBe('Everywhere');
  expect(groups[0].labels).toEqual(['Search all clusters for “dply”']);
  await expect(page.locator(activeLabel)).toHaveText('Search all clusters for “dply”');

  // ⏎ on the fresh query routes to the all-clusters search (D12).
  await page.keyboard.press('Enter');
  await page.waitForURL(/\/search\?q=dply$/);
  await expect(page.locator('.search-big input[name="q"]')).toHaveValue('dply');
});

test('group order: Everywhere -> On this page -> Resource types (typing); standard groups on empty', async ({
  page,
}) => {
  await page.goto(PODS);
  await openPalette(page);

  // Empty query, fresh profile: no Recents yet, the standard groups in SPEC
  // §6.3 order, Everywhere absent.
  let groups = await paletteGroups(page);
  expect(groups.map((g) => g.title)).toEqual([
    'On this page',
    'Resource types',
    'Namespaces',
    'Clusters',
    'Actions',
  ]);

  // "ng" matches a page object (nginx) and resource types (Ingresses…): the
  // group order while typing is Everywhere, then On this page, then the rest.
  await page.locator('#ro-palette-input').fill('ng');
  groups = await paletteGroups(page);
  expect(groups.map((g) => g.title).slice(0, 3)).toEqual([
    'Everywhere',
    'On this page',
    'Resource types',
  ]);
  expect(groups[1].labels).toContain('nginx');
});

test('fuzzy subsequence ranking: prefix > word-start > scattered; dply lands Deployments first', async ({
  page,
}) => {
  await page.goto(PODS);

  // The ranker is a pure function -- unit-test the tier ordering in isolation
  // through the window.roFuzzy seam (no palette DOM involved).
  const scores = await page.evaluate(() => {
    const f = (window as any).roFuzzy as (q: string, t: string) => number;
    return {
      prefix: f('sys', 'system-pods'), // contiguous from char 0
      wordStart: f('sys', 'kube-system'), // contiguous from the "-" boundary
      camelHump: f('vol', 'PersistentVolumes'), // contiguous from a camel hump
      scattered: f('sys', 'misty-sales'), // s…y…s spread mid-word
      subsequence: f('dply', 'Deployments'), // scattered but early + tight
      none: f('sys', 'pods'), // not a subsequence
      substringGone: f('dply', 'dp-ly-zz'), // subsequence works where substring would fail
    };
  });
  expect(scores.none).toBe(-1);
  expect(scores.subsequence).toBeGreaterThanOrEqual(0);
  expect(scores.substringGone).toBeGreaterThanOrEqual(0);
  expect(scores.prefix).toBeLessThan(scores.wordStart);
  expect(scores.wordStart).toBeLessThan(scores.scattered);
  expect(scores.camelHump).toBeLessThan(scores.scattered);

  // Live palette: "dply" is not a substring of anything, yet Deployments tops
  // its group (the substring matcher this replaces found nothing here).
  await openPalette(page);
  await page.locator('#ro-palette-input').fill('dply');
  let groups = await paletteGroups(page);
  let types = groups.find((g) => g.title === 'Resource types');
  expect(types, 'dply must surface the Resource types group').toBeTruthy();
  expect(types!.labels[0]).toBe('Deployments');

  // "po": the prefix match (Pods) leads, and the tight scattered match
  // (Deployments: p·o spans 3 chars) ranks above the wide one
  // (PersistentVolumes: p…o spans 12).
  await page.locator('#ro-palette-input').fill('po');
  groups = await paletteGroups(page);
  types = groups.find((g) => g.title === 'Resource types');
  expect(types!.labels[0]).toBe('Pods');
  const dep = types!.labels.indexOf('Deployments');
  const pv = types!.labels.indexOf('PersistentVolumes');
  expect(dep).toBeGreaterThan(-1);
  expect(pv).toBeGreaterThan(-1);
  expect(dep).toBeLessThan(pv);
});

test('choosing entries builds the persisted Recents group: newest first, deduped, across reload', async ({
  page,
}) => {
  await page.goto(PODS);

  // Fresh profile: no Recents group on the empty query.
  await openPalette(page);
  let groups = await paletteGroups(page);
  expect(groups.map((g) => g.title)).not.toContain('Recents');

  // Choose the Nodes resource type (↓ steps off the Everywhere row).
  await page.locator('#ro-palette-input').fill('nodes');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator(activeLabel)).toHaveText('Nodes');
  await page.keyboard.press('Enter');
  await page.waitForURL(/\/clusters\/e2e\/nodes$/);

  // Reopening empty now leads with Recents carrying the choice.
  await openPalette(page);
  groups = await paletteGroups(page);
  expect(groups[0].title).toBe('Recents');
  expect(groups[0].labels).toEqual(['Nodes']);

  // A second choice (Pods, from the cluster-scoped page -> the _all list)
  // stacks newest-first.
  await page.locator('#ro-palette-input').fill('pods');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator(activeLabel)).toHaveText('Pods');
  await page.keyboard.press('Enter');
  await page.waitForURL(/\/clusters\/e2e\/namespaces\/_all\/pods$/);
  await openPalette(page);
  groups = await paletteGroups(page);
  expect(groups[0].title).toBe('Recents');
  expect(groups[0].labels).toEqual(['Pods', 'Nodes']);

  // Re-choosing Nodes FROM the Recents group moves it back to the front
  // without duplicating it (dedupe by destination).
  await page.keyboard.press('ArrowDown'); // Pods (recents[0]) -> Nodes (recents[1])
  await expect(page.locator(activeLabel)).toHaveText('Nodes');
  await page.keyboard.press('Enter');
  await page.waitForURL(/\/clusters\/e2e\/nodes$/);
  await openPalette(page);
  groups = await paletteGroups(page);
  expect(groups[0].title).toBe('Recents');
  expect(groups[0].labels).toEqual(['Nodes', 'Pods']);

  // localStorage persistence: a full reload keeps the same Recents.
  await page.keyboard.press('Escape');
  await page.reload();
  await openPalette(page);
  groups = await paletteGroups(page);
  expect(groups[0].title).toBe('Recents');
  expect(groups[0].labels).toEqual(['Nodes', 'Pods']);
});

test('the Actions group keeps the v1 content on a detail page: tab jumps, Logs, Toggle theme', async ({
  page,
}) => {
  await page.goto(`${PODS}/nginx`);
  await openPalette(page);

  // The retention pin (review-flagged path): the rebuilt palette still carries
  // the v1 Actions content -- the detail-tab jumps for the object in scope,
  // the workload Logs jump, the theme toggle, and the All-clusters jump.
  const groups = await paletteGroups(page);
  const actions = groups.find((g) => g.title === 'Actions');
  expect(actions, 'the Actions group must render on a detail page').toBeTruthy();
  for (const label of ['Default view', 'YAML', 'Events', 'Logs', 'Toggle theme', 'All clusters']) {
    expect(actions!.labels, `Actions group must keep "${label}"`).toContain(label);
  }

  // And the retained Logs jump is live end to end: ⌘K -> logs -> ⏎ opens the
  // logs sub-page of the object in scope.
  await page.locator('#ro-palette-input').fill('logs');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator(activeLabel)).toHaveText('Logs');
  await page.keyboard.press('Enter');
  await page.waitForURL(/\/clusters\/e2e\/namespaces\/default\/pods\/nginx\/logs$/);
});

test('resource-type rows carry the kind icon, API-group meta, and the quiet scope badge', async ({
  page,
}) => {
  await page.goto(PODS);
  await openPalette(page);

  // A namespaced CRD: Certificates shows its API group and the "namespaced"
  // badge (the Unit 3 scope-badge wording, not the old NS abbreviation).
  await page.locator('#ro-palette-input').fill('certificates');
  const certRow = page
    .locator('#ro-palette-list .ro-pal-item')
    .filter({ has: page.locator('.pal-label', { hasText: /^Certificates$/ }) });
  await expect(certRow).toHaveCount(1);
  await expect(certRow.locator('.pal-meta')).toHaveText('cert-manager.io');
  await expect(certRow.locator('.pal-scope')).toHaveText('namespaced');
  expect(await certRow.locator('.ico, .kind-tile').count()).toBeGreaterThan(0);

  // A cluster-scoped built-in: Nodes carries the "cluster" badge variant.
  await page.locator('#ro-palette-input').fill('nodes');
  const nodesRow = page
    .locator('#ro-palette-list .ro-pal-item')
    .filter({ has: page.locator('.pal-label', { hasText: /^Nodes$/ }) });
  await expect(nodesRow).toHaveCount(1);
  await expect(nodesRow.locator('.pal-scope')).toHaveText('cluster');
  await expect(nodesRow.locator('.pal-scope')).toHaveClass(/cluster/);
});
