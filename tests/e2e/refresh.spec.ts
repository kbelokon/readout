import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Auto-refresh v2, made deterministic by the fakeapi
// control surface:
//
//   - a failed tick keeps the last-good rows (dimmed), reveals the warn
//     banner, and COUNTS DOWN live to the next retry;
//   - repeated failures back off 1x -> 2x (-> 4x, capped 60s) of the chosen
//     interval: the second failure visibly doubles the wait;
//   - Retry now fires immediately -- it never waits out the backoff;
//   - the first success after failures clears the banner + dim and fires the
//     second sanctioned toast, "Refresh resumed";
//   - a scripted LIST mutation flashes the changed cell on the next tick: the
//     polling-mode flash net, so severing Live mode can never sever the
//     only flash coverage;
//   - the interval choice persists via the ro_prefs cookie (asserted on the
//     NEW 10s option that replaced the old 15s default);
//   - the topbar livedot pulses while any non-Off interval is active and is
//     static GHOST-GREY at Off (the brand dot pulses only when live; and per
//     the colour law brand-green is a live-health signal, so a dot that
//     stays green at Off would be a false signal).

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

// A tick (or any programmatic re-fetch, the Retry-now click included) marks
// itself RO-No-Push; matching on the request header keeps it awaitable apart
// from user sorts and preload warm-ups (the list-loop.spec.ts pattern).
function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

function waitForFailedTick(page: Page): Promise<Response> {
  return page.waitForResponse((r) => isTickResponse(r) && !r.ok(), { timeout: 15_000 });
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

// The navbar interval menu opens on hover (CSS :hover/:focus-within).
async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-ro-interval="${secs}"]`).click();
}

// Resolve a CSS custom property to the computed rgb() serialization toHaveCSS
// compares against (the raw token is a hex literal; themes differ, so the
// expected colour is read from the page itself, never hardcoded).
function resolvedToken(page: Page, token: string): Promise<string> {
  return page.evaluate((t) => {
    const probe = document.createElement('span');
    probe.style.color = `var(${t})`;
    document.body.appendChild(probe);
    const rgb = getComputedStyle(probe).color;
    probe.remove();
    return rgb;
  }, token);
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the auto-refresh chrome is a desktop surface (below 760px the card layer replaces the table, D22)'
  );
  await control('/__control/reset');
});

test('failure backs off with a live countdown, Retry now is immediate, recovery toasts', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  await pickInterval(page, 5);
  // Park the cursor mid-page: the hover-opened refresh menu would otherwise
  // stay open over the banner and intercept the Retry-now click below.
  await page.mouse.move(200, 400);
  await control('/__control/fail-lists?mode=500');

  // FIRST failed tick: rows dim but never disappear; the banner reveals with
  // a live countdown aimed at the 1x retry (<= the 5s base interval). The
  // SECOND failed-tick waiter is armed IMMEDIATELY: the 1x retry fires ~5s
  // out, and registering the waiter only after the assertion block below
  // could miss tick 2 under load and resolve on tick 3 instead -- whose
  // countdown re-arms at 4x = 20s, where a 10s-shaped doubling check can
  // never match.
  await waitForFailedTick(page);
  const secondFailed = waitForFailedTick(page);
  const banner = page.locator('.ro-stale-banner');
  await expect(banner).toBeVisible();
  await expect(page.locator('#resource-list-content')).toHaveClass(/ro-stale/);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  const countdown = banner.locator('[data-stale-countdown]');
  await expect(countdown).toHaveText(/^[1-5]s$/);

  // SECOND failed tick (the 1x retry): the wait DOUBLES to 2x = 10s. The
  // countdown is a DECREASING counter, so the doubling is proven with a
  // floor -- anything above 5s is impossible at the 1x cadence -- instead of
  // pinning exact text whose match window is 3 wall-clock seconds.
  await secondFailed;
  await expect
    .poll(async () => parseInt((await countdown.textContent()) ?? '0', 10), { timeout: 4_000 })
    .toBeGreaterThan(5);
  // ... and the countdown is LIVE: it decrements on the banner.
  const first = parseInt((await countdown.textContent()) ?? '0', 10);
  await expect
    .poll(async () => parseInt((await countdown.textContent()) ?? '0', 10), { timeout: 4_000 })
    .toBeLessThan(first);

  // Retry now fires IMMEDIATELY -- most of the doubled backoff wait still
  // remains, but the re-fetch (a programmatic RO-No-Push request) lands
  // within moments.
  await control('/__control/fail-lists?mode=off');
  const retried = page.waitForResponse(isTickResponse, { timeout: 3_000 });
  await banner.locator('.ro-stale-retry').click();
  await retried;

  // Recovery: banner clears, dim lifts, rows intact -- and the SECOND
  // sanctioned toast announces it (recovery-only, never per-tick).
  await expect(banner).toBeHidden();
  await expect(page.locator('#resource-list-content')).not.toHaveClass(/ro-stale/);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  await expect(page.locator('#ro-toasts .ro-toast')).toHaveText('Refresh resumed');
});

test('a scripted status change flashes the changed cell on the next polling tick', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);

  // Mutate the fixture's LIST state per the fakeapi contract (control-applied
  // changes alter subsequent LIST responses): nginx's READY/
  // STATUS/RESTARTS cells change; NAME and AGE stay byte-identical.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: 'nginx', namespace: 'default', uid: '00000000-0000-0000-0000-000000000001' },
        status: { phase: 'Running' },
      },
      cells: ['nginx', '0/1', 'CrashLoopBackOff', '3', '10m'],
    },
  ]);
  await pickInterval(page, 5);
  await waitForTick(page);

  // The morph surfaced the change honestly: the STATUS cell flashes
  // ro-cell-changed, the unchanged NAME cell does not, and the untouched
  // my-app row carries no flash at all.
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator('td:has(span.cell-status)')).toHaveClass(/ro-cell-changed/);
  await expect(nginx.locator('td.cell-name')).not.toHaveClass(/ro-cell-changed/);
  await expect(page.locator('tr[data-key="e2e/default/my-app"] td.ro-cell-changed')).toHaveCount(0);
});

test('the interval choice (the new 10s option) survives reload via the prefs cookie', async ({
  page,
}) => {
  await page.goto(PODS);
  // 10s replaced the old 15s -- the dropdown offers exactly Off/5/10/30/60, plus
  // the Live stream mode.
  await expect(page.locator('.refresh-menu .refresh-option')).toHaveText([
    'Off',
    'Every 5s',
    'Every 10s',
    'Every 30s',
    'Every 60s',
    'Live',
  ]);
  await expect(page.locator('.refresh-option[data-ro-interval="15"]')).toHaveCount(0);

  await pickInterval(page, 10);
  await expect(page.locator('#refresh-label')).toHaveText('10s');

  // A fresh server render carries the persisted mode at SSR (the ro_prefs
  // cookie is the only carrier across this reload).
  await page.reload();
  await expect(page.locator('#refresh-label')).toHaveText('10s');
  await expect(page.locator('.refresh-option[data-ro-interval="10"]')).toHaveClass(/is-active/);
});

test('the refresh menu opens on hover and survives the pointer travelling into it', async ({
  page,
}) => {
  await page.goto(PODS);
  const trigger = page.locator('#refresh-dropdown .refresh-trigger');
  const menu = page.locator('.refresh-menu');
  const option = page.locator('.refresh-option[data-ro-interval="30"]');

  // Hover alone reveals the menu -- no click required.
  await trigger.hover();
  await expect(menu).toBeVisible();

  // The pointer must be able to TRAVEL from the trigger into the menu: walk
  // the real path in small steps (crossing the visual gap under the trigger).
  // Without a hover bridge over that gap the menu closes mid-travel.
  const from = (await trigger.boundingBox())!;
  const to = (await option.boundingBox())!;
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
  await page.mouse.move(to.x + to.width / 2, to.y + to.height / 2, { steps: 25 });
  await expect(menu).toBeVisible();
  await expect(option).toBeVisible();

  // And the option is directly clickable from the hover-opened state.
  await option.click();
  await expect(page.locator('#refresh-label')).toHaveText('30s');
});

test('the livedot pulses brand while an interval is active and is static ghost at Off', async ({
  page,
}) => {
  await page.goto(PODS);
  const dot = page.locator('#refresh-dropdown .ro-livedot');
  const ghost = await resolvedToken(page, '--text-ghost');
  const brand = await resolvedToken(page, '--brand');
  expect(ghost).not.toBe(brand); // the colour assertions below must be able to tell them apart

  // Off (the default): a static GHOST dot -- no pulse AND no brand green
  // (colour law §1.1: green means live health; a green dot at Off is a false
  // signal -- the prototype defaults the dot to --text-ghost, chrome.css:110).
  await expect(dot).toHaveCSS('animation-name', 'none');
  await expect(dot).toHaveCSS('background-color', ghost);

  // An active interval: brand colour + the pulse, both under the one
  // refresh-on state owner (SSR refreshDropdownClass / JS syncRefreshUI).
  await pickInterval(page, 5);
  await expect(dot).toHaveCSS('animation-name', 'ro-pulse');
  await expect(dot).toHaveCSS('background-color', brand);

  // Back to Off: the dot drops the pulse AND the green.
  await pickInterval(page, 0);
  await expect(dot).toHaveCSS('animation-name', 'none');
  await expect(dot).toHaveCSS('background-color', ghost);
});
