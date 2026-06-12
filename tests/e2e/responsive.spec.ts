import { test, expect } from '@playwright/test';
import { controlURL } from './playwright.config';

// Responsive breakpoints, driven by REAL viewport
// switching on one page -- breakpoint behavior is viewport-dependent rendering,
// so resizing the same Chromium page is the only honest check:
//
//   >1100px        full sidebar (icons + labels + counts);
//   760.02-1100px  the `.ro-rail` icon rail: labels/counts hidden, icons stay,
//                  each entry named via its title tooltip, the table layer
//                  still renders with the name column reachable;
//   ≤760px         the shipped mobile card layer is KEPT: `.ro-pcard`
//                  cards instead of the table -- never a dead-end note -- plus
//                  the hamburger that reveals a NON-EMPTY sidebar panel (the
//                  legacy `aside #aside-menu{display:none}` rule used to blank
//                  it; this guards the removal);
//   back >1100px   the full sidebar returns (no one-way state).
//
// Runs on the desktop project only: the spec OWNS the viewport via
// page.setViewportSize, so the mobile project would just duplicate it.

const PODS = '/clusters/e2e/namespaces/default/pods';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the spec drives every breakpoint itself via setViewportSize'
  );
  const res = await fetch(`${controlURL}/__control/reset`);
  if (!res.ok) {
    throw new Error(`control reset: ${res.status} ${await res.text()}`);
  }
});

test('sidebar walks full -> icon rail -> mobile cards -> full across the breakpoints', async ({
  page,
}) => {
  await page.goto(PODS);

  const sidebar = page.locator('aside.ro-sidebar');
  const podsLink = page.locator('.ro-sidebar .menu-list a[title="Pods"]');
  const podsLabel = podsLink.locator('.nav-label');

  // --- 1280px: the full sidebar (labels visible, wide aside, table layer). ---
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(sidebar).toBeVisible();
  await expect(podsLabel).toBeVisible();
  await expect(podsLabel).toHaveText('Pods');
  // Counts are visible at full width (proves the 1000px all-hidden loop below
  // iterates a real, non-empty set).
  await expect(page.locator('.ro-sidebar .menu-count').first()).toBeVisible();
  await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
  const fullWidth = (await sidebar.boundingBox())!.width;
  expect(fullWidth).toBeGreaterThan(180); // --sidebar-w is 224px

  // --- 1000px: the icon rail. ---
  await page.setViewportSize({ width: 1000, height: 800 });
  await expect(sidebar).toBeVisible();
  // Icons without text labels: the label and the count collapse, the icon slot
  // stays, and the entry keeps its name as a title tooltip.
  await expect(podsLabel).toBeHidden();
  await expect(podsLink.locator('.ico, .kind-tile, .kind-img, .kind-emoji').first()).toBeVisible();
  for (const count of await page.locator('.ro-sidebar .menu-count').all()) {
    await expect(count).toBeHidden();
  }
  const railWidth = (await sidebar.boundingBox())!.width;
  expect(railWidth).toBeLessThan(80);
  expect(railWidth).toBeGreaterThan(40);
  // The rail entry still navigates (it is the same anchor, just icon-only).
  await expect(podsLink).toHaveAttribute('href', PODS);
  // Lists render under the rail: the table layer with the name column reachable.
  await expect(page.locator('table.ro-table td.cell-name a').first()).toBeVisible();
  // No hamburger at rail width -- the rail itself is the navigation.
  await expect(page.locator('.menu-toggle')).toBeHidden();

  // --- 700px: the mobile card layer -- cards, NOT a dead-end note. ---
  await page.setViewportSize({ width: 700, height: 800 });
  await expect(page.locator('.ro-cardlist .ro-pcard').first()).toBeVisible();
  await expect(page.locator('.ro-table-wrap.has-cards')).toBeHidden();
  // The card carries the loud identifier, so the name stays reachable here too.
  await expect(page.locator('.ro-pcard .pc-name a').first()).toBeVisible();
  await expect(page.locator('.ro-pcard .pc-name a').first()).toHaveText(/nginx|my-app/);
  // The sidebar hides; the hamburger reveals a NON-EMPTY overlay panel.
  await expect(sidebar).toBeHidden();
  const toggle = page.locator('header.ro-topbar .menu-toggle');
  await expect(toggle).toBeVisible();
  await toggle.click();
  await expect(sidebar).toBeVisible();
  await expect(page.locator('#aside-menu .menu-item').first()).toBeVisible();
  await expect(podsLabel).toBeVisible();
  // Close it again (the toggle is symmetric) before leaving the breakpoint.
  await toggle.click();
  await expect(sidebar).toBeHidden();

  // --- back to 1280px: the full sidebar returns. ---
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(sidebar).toBeVisible();
  await expect(podsLabel).toBeVisible();
  await expect(page.locator('table.ro-table td.cell-name').first()).toBeVisible();
  expect((await sidebar.boundingBox())!.width).toBeGreaterThan(180);
});

test('mobile cards speak the v2 cell vocabulary at 700px', async ({ page }) => {
  // The Go suite pins the card DOM contract (mobile_cards_test.go); this checks
  // the half a DOM test cannot: at a real ≤760px viewport the v2-vocabulary
  // card cells are the VISIBLE layer (tones, ready ratio, keyed meta rows).
  await page.setViewportSize({ width: 700, height: 800 });
  await page.goto(PODS);

  const nginxCard = page.locator('.ro-pcard', {
    has: page.locator(`.pc-name a[href="${PODS}/nginx"]`),
  });
  await expect(nginxCard).toBeVisible();
  // Status pill: tone class + dot (status is never colour alone -- dot + word).
  await expect(nginxCard.locator('.pc-status.ok .ro-dot.ok')).toBeVisible();
  await expect(nginxCard.locator('.pc-status.ok')).toContainText('Running');
  // Ready ratio + keyed meta rows in the same vocabulary the table uses.
  await expect(nginxCard.locator('.pc-meta .ready.full')).toHaveText('1/1');
  await expect(nginxCard.locator('.pc-meta .m .k', { hasText: 'ready' })).toBeVisible();
});

test('a long sidebar kind name squeezes with an ellipsis instead of pushing the count out', async ({
  page,
}) => {
  await page.goto('/clusters/e2e/namespaces/default/pods');
  // Keyed by href, not text: the probe below renames the label, and a
  // text-keyed locator would re-resolve to nothing afterwards.
  const link = page.locator('.ro-sidebar .menu-list a[href$="/pods"]').first();
  // Force the failure shape from the field report: a kind label as long as
  // HorizontalPodAutoscalers, next to a real count badge.
  await link.evaluate((a) => {
    a.querySelector('.nav-label')!.textContent = 'HorizontalPodAutoscalersOverflowProbe';
    let count = a.querySelector('.menu-count');
    if (!count) {
      count = document.createElement('span');
      count.className = 'menu-count';
      a.appendChild(count);
    }
    count.textContent = '22';
  });
  const aside = (await page.locator('.ro-sidebar').boundingBox())!;
  const count = (await link.locator('.menu-count').boundingBox())!;
  // The badge stays fully inside the sidebar...
  expect(count.x + count.width).toBeLessThanOrEqual(aside.x + aside.width);
  // ...because the LABEL is what gave way (scrollWidth > clientWidth = clipped).
  const clipped = await link
    .locator('.nav-label')
    .evaluate((el) => el.scrollWidth > el.clientWidth);
  expect(clipped).toBe(true);
});
