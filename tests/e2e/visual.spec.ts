import { test, expect, type Page } from '@playwright/test';
import { controlURL } from './playwright.config';

// SPEC §9 visual walk: pixel baselines over the deterministic fakeapi harness,
// the pixel insurance that licenses every later refactor (Unit 2). Any
// unintended visual drift fails this run.
//
// CRITICAL — these baselines are generated ONLY inside the pinned Playwright
// container (`make e2e-visual-update`), never on the arm64-macOS host: the
// host renderer does not match the linux/amd64 container, so host-grown
// baselines would diff on every CI/container run. The host `make e2e` runs the
// desktop/mobile projects only and never touches this spec (testIgnore in
// playwright.config.ts).
//
// Determinism contract (why this can run twice with a zero diff):
//   - the fakeapi fixtures carry FIXED Table cells, ages, and timestamps
//     (formatTimestamp only reformats; nothing here is relative-to-now), so
//     the only honestly dynamic surface is the search timing meter — masked
//     below with an explicit, justified comment;
//   - `reducedMotion: 'reduce'` (set on the `visual` project, NOT the global
//     use{}) freezes the cell-flash and the live-dot pulse — animations that
//     other behavioural specs assert and that a global flag would break;
//   - `document.fonts.ready` is awaited before every snapshot so the
//     self-hosted Geist / Geist Mono faces are painted, never a fallback;
//   - the list refresh defaults to Off, so no polling/stream tick fires under
//     a snapshot; the windowed list is scrolled to a stable top window first.
//
// Strict comparison: no global maxDiffPixels / threshold. The single mask is
// the search timing meter (`.ro-phase-meta`, "searched M clusters in T s") —
// honestly wall-clock dynamic; everything around it is asserted pixel-exact.

const PODS = '/clusters/e2e/namespaces/default/pods';
const NODES = '/clusters/e2e/nodes';
const NAMESPACES = '/clusters/e2e/namespaces';
const POD = '/clusters/e2e/namespaces/default/pods/nginx';
const LOGS = `${POD}/logs`;
const BIG_PODS = '/clusters/e2e/namespaces/big/pods';
const CLUSTERS = '/clusters';
// Scope the search to the pods type the fakeapi serves so the result group is
// deterministic: the default type set fans out to kinds the fixture does not
// answer, which renders a (non-deterministic) partial-failure banner with no
// results instead of a populated group.
const SEARCH = '/search?cluster=e2e&q=nginx&type=pods';

// The three SPEC §8.5 viewport bands. 900px sits in the 760–1100px icon-rail
// band (invisible to both the 1440px desktop frame and the 390px mobile
// frame), so the rail chrome only ever shows here.
const VIEWPORTS = {
  desktop: { width: 1440, height: 900 },
  mobile: { width: 390, height: 844 },
  rail: { width: 900, height: 900 },
} as const;
type ViewportName = keyof typeof VIEWPORTS;

// The dense-frame budget for the two text-heavy nodes tables. Rosetta's
// glyph-edge noise is BIMODAL (see playwright.config.ts): ~100 px or fewer on
// the 32 clean frames vs ~6326 on the nodes table. The default RO_VISUAL_MAXDIFF
// (project-level) keeps the 32 clean frames tight; these two frames take
// RO_VISUAL_MAXDIFF_DENSE instead, falling back to RO_VISUAL_MAXDIFF, default 0
// (strict — CI sets neither env, so the canonical native-amd64 check stays
// zero-tolerance on every frame).
const DENSE_MAXDIFF = Number(
  process.env.RO_VISUAL_MAXDIFF_DENSE ?? process.env.RO_VISUAL_MAXDIFF ?? 0
);

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

// setTheme writes the production `theme` cookie (the same write the
// preferences POST and the topbar toggle perform — see prefs-persistence.spec
// and preferences.templ). An explicit cookie makes the server stamp
// <html data-theme=…>, so the snapshot renders the chosen palette at SSR with
// no client flash.
async function setTheme(page: Page, theme: 'light' | 'dark'): Promise<void> {
  const url = new URL(controlURL);
  await page.context().addCookies([
    { name: 'theme', value: theme, domain: url.hostname, path: '/' },
  ]);
}

// settle awaits the self-hosted fonts before a snapshot. Without it the first
// paint can land a fallback face and the baseline would be font-metric noisy.
async function settle(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
}

// snap takes ONE full-page baseline keyed `<viewport>-<theme>-<frame>.png`.
// animations:'disabled' is belt-and-suspenders over the project reducedMotion;
// caret:'hide' keeps a blinking text caret out of any focused-input frame.
async function snap(
  page: Page,
  viewport: ViewportName,
  theme: 'light' | 'dark',
  frame: string,
  opts: Parameters<ReturnType<typeof expect>['toHaveScreenshot']>[1] = {}
): Promise<void> {
  await settle(page);
  await expect(page).toHaveScreenshot(`${viewport}-${theme}-${frame}.png`, {
    fullPage: true,
    animations: 'disabled',
    caret: 'hide',
    ...opts,
  });
}

const THEMES = ['light', 'dark'] as const;

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'visual',
    'visual baselines are container-only; the desktop/mobile projects ignore this file'
  );
  await control('/__control/reset');
});

// ---------------------------------------------------------------------------
// Page frames — both themes on the desktop band (the table chrome lives here).
// Tokens: clusters, namespaces, pods, nodes, detail-default, detail-yaml,
// logs, search, empty, forbidden, unreachable, windowed.
// ---------------------------------------------------------------------------

for (const theme of THEMES) {
  test(`clusters page — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(CLUSTERS);
    await expect(page.locator('.ro-topbar')).toBeVisible();
    await snap(page, 'desktop', theme, 'clusters');
  });

  test(`namespaces list — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(NAMESPACES);
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    await snap(page, 'desktop', theme, 'namespaces');
  });

  test(`pods list — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(PODS);
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    await snap(page, 'desktop', theme, 'pods');
  });

  test(`nodes list (cluster-scoped) — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(NODES);
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    // The dense-table frame: takes the wider DENSE_MAXDIFF budget for the
    // Rosetta glyph-edge outlier (see DENSE_MAXDIFF above). Overrides the
    // project-level default maxDiffPixels for this comparison only.
    await snap(page, 'desktop', theme, 'nodes', { maxDiffPixels: DENSE_MAXDIFF });
  });

  test(`detail-default view — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(POD);
    await expect(page.locator('.ro-detail-title .pn-head')).toHaveText('nginx');
    await snap(page, 'desktop', theme, 'detail-default');
  });

  test(`detail-yaml view — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(`${POD}?view=yaml`);
    // The ?view=yaml tab renders the single highlighted document
    // (.ro-yaml-view); the per-section .ro-yaml-card stack is the DEFAULT tab.
    await expect(page.locator('.ro-yaml-view')).toBeVisible();
    await snap(page, 'desktop', theme, 'detail-yaml');
  });

  test(`logs page — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(LOGS);
    await expect(page.locator('pre.ro-logpre')).toBeVisible();
    await expect(page.locator('.ro-logpre .log-line').first()).toBeVisible();
    await snap(page, 'desktop', theme, 'logs');
  });

  test(`search results — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(SEARCH);
    await expect(page.locator('.search-group table.ro-table').first()).toBeVisible();
    // MASK (honest dynamic): the totals meter ".ro-phase-meta" reads
    // "searched M clusters in T s" — T is wall-clock search latency, different
    // every run. Nothing else on the page is time-derived (the fixture ages
    // and result rows are fixed), so masking only this span keeps the rest of
    // the search frame pixel-exact.
    await snap(page, 'desktop', theme, 'search', {
      mask: [page.locator('.ro-phase-meta')],
    });
  });

  test(`empty-filtered list — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(`${PODS}?filter=zzz-no-such-pod`);
    await expect(page.locator('.ro-empty-lg h3')).toBeVisible();
    await snap(page, 'desktop', theme, 'empty');
  });

  test(`forbidden whole-list state — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await control('/__control/fail-lists?mode=403');
    await page.goto(PODS);
    await expect(page.locator('.ro-empty-lg .ro-empty-glyph.warn')).toBeVisible();
    await snap(page, 'desktop', theme, 'forbidden');
  });

  test(`unreachable whole-list state — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await control('/__control/fail-lists?mode=500');
    await page.goto(PODS);
    await expect(page.locator('.ro-empty-lg .ro-empty-glyph.err')).toBeVisible();
    await snap(page, 'desktop', theme, 'unreachable');
  });

  test(`windowed big list — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.goto(BIG_PODS);
    // Stabilize the window: pin the scroll to the top, then wait for the
    // virtualizer's top slice (big-pod-0001 visible, exactly two spacer rows).
    await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));
    await expect(
      page.locator('tr[data-key="e2e/big/big-pod-0001"]')
    ).toBeVisible();
    await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
    // Full-page on a 600-row windowed list would chase a moving virtual
    // window mid-capture; clip to the fixed viewport so the captured slice is
    // exactly the stable top window.
    await snap(page, 'desktop', theme, 'windowed', { fullPage: false });
  });
}

// ---------------------------------------------------------------------------
// Mobile band — the card layer (D22) replaces the table below 760px.
// Token: mobile-cards. Both themes.
// ---------------------------------------------------------------------------

for (const theme of THEMES) {
  test(`mobile-cards pods — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.setViewportSize(VIEWPORTS.mobile);
    await page.goto(PODS);
    await expect(page.locator('.ro-cardlist .ro-pcard').first()).toBeVisible();
    await snap(page, 'mobile', theme, 'mobile-cards');
  });
}

// ---------------------------------------------------------------------------
// Rail band — the 900px icon rail (760–1100px), invisible to the other two
// frames. Token: rail. Both themes.
// ---------------------------------------------------------------------------

for (const theme of THEMES) {
  test(`rail sidebar pods — ${theme}`, async ({ page }) => {
    await setTheme(page, theme);
    await page.setViewportSize(VIEWPORTS.rail);
    await page.goto(PODS);
    // The 760–1100px band collapses the sidebar to the icon rail.
    await expect(page.locator('aside.ro-sidebar')).toBeVisible();
    await expect(page.locator('.ro-sidebar .menu-list a[title="Pods"] .nav-label')).toBeHidden();
    await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
    await snap(page, 'rail', theme, 'rail');
  });
}

// ---------------------------------------------------------------------------
// Interaction-state frames — captured by an explicit action, in ONE theme
// where the state itself (not the palette) is the subject. Desktop band.
// Tokens: hover-row, focus-visible, bulk-bar, cols-popover, palette,
// filter-chip.
// ---------------------------------------------------------------------------

test('hover-row state', async ({ page }) => {
  await setTheme(page, 'dark');
  await page.goto(PODS);
  const row = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(row).toBeVisible();
  await row.hover();
  await snap(page, 'desktop', 'dark', 'hover-row');
});

test('focus-visible state', async ({ page }) => {
  await setTheme(page, 'light');
  await page.goto(PODS);
  await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
  // j focuses the first row through the keyboard walker — the focus-visible
  // ring is a keyboard-only affordance.
  await page.keyboard.press('j');
  await expect(page.locator('tr.kfocus')).toHaveCount(1);
  await snap(page, 'desktop', 'light', 'focus-visible');
});

test('bulk-bar state', async ({ page }) => {
  await setTheme(page, 'dark');
  await page.goto(PODS);
  // Row-click on the plain Ready cell toggles selection without opening the row.
  await page.locator('tr[data-key="e2e/default/nginx"] td').nth(1).click();
  await page.locator('tr[data-key="e2e/default/my-app"] td').nth(1).click();
  await expect(page.locator('#ro-bulkbar')).toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');
  await snap(page, 'desktop', 'dark', 'bulk-bar');
});

test('cols-popover state', async ({ page }) => {
  await setTheme(page, 'light');
  await page.goto(NODES);
  await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
  await page.locator('#ro-cols-btn').click();
  await expect(page.locator('#ro-cols-pop')).toBeVisible();
  await snap(page, 'desktop', 'light', 'cols-popover');
});

test('palette state', async ({ page }) => {
  await setTheme(page, 'dark');
  await page.goto(PODS);
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('#ro-palette')).toHaveClass(/open/);
  // A fixed query so the rendered groups/rows are deterministic.
  await page.locator('#ro-palette-input').fill('dply');
  await expect(page.locator('#ro-palette-list .ro-pal-item.active .pal-label')).toBeVisible();
  await snap(page, 'desktop', 'dark', 'palette');
});

test('filter-chip state', async ({ page }) => {
  await setTheme(page, 'light');
  await page.goto(`${PODS}?f=status%3ARunning`);
  // The chip is server-rendered from the f= deep link, so the editor field
  // state is deterministic without a typed/committed interaction.
  await expect(page.locator('#ro-filter-field .ro-scope-chip')).toHaveCount(1);
  await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
  await snap(page, 'desktop', 'light', 'filter-chip');
});
