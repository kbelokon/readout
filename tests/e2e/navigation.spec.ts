import { expect, test, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Navigation must never animate or expose a half-initialized page. These tests
// exercise the real boosted HTMX routes in Chromium and wrap the browser's
// native View Transition entry point only to prove that Readout never calls it.
// The YAML assertion samples the first rendering opportunity after a body swap:
// fold controls and their layout-neutral gutter must already be complete then.

const PODS = '/clusters/e2e/namespaces/default/pods';
const POD = '/clusters/e2e/namespaces/default/pods/nginx';
const SERVICES = '/clusters/e2e/namespaces/default/services';
const NAMESPACES = '/clusters/e2e/namespaces';

interface NavigationProbe {
  nativeStarts: number;
  beforeTransitions: number;
  afterRequests: number;
  afterSwaps: number;
  historyCacheHits: number;
  firstPaint: null | {
    folds: number;
    directToggle: boolean;
    controlPosition: string | null;
    tokenX: number | null;
  };
}

async function control(path: string): Promise<void> {
  const response = await fetch(controlURL + path);
  if (!response.ok) {
    throw new Error(`control ${path}: ${response.status} ${await response.text()}`);
  }
}

async function installNavigationProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const probe: NavigationProbe = {
      nativeStarts: 0,
      beforeTransitions: 0,
      afterRequests: 0,
      afterSwaps: 0,
      historyCacheHits: 0,
      firstPaint: null,
    };
    (window as unknown as { __navigationProbe: NavigationProbe }).__navigationProbe = probe;

    document.addEventListener('htmx:beforeTransition', () => {
      probe.beforeTransitions += 1;
    });
    document.addEventListener('htmx:afterRequest', () => {
      probe.afterRequests += 1;
    });
    document.addEventListener('htmx:afterSwap', () => {
      probe.afterSwaps += 1;
    });
    document.addEventListener('htmx:historyCacheHit', () => {
      probe.historyCacheHits += 1;
    });

    const nativeStart = document.startViewTransition;
    if (typeof nativeStart === 'function') {
      Object.defineProperty(document, 'startViewTransition', {
        configurable: true,
        value: function startViewTransition(
          this: Document,
          ...args: Parameters<Document['startViewTransition']>
        ) {
          probe.nativeStarts += 1;
          return Reflect.apply(nativeStart, this, args);
        },
      });
    }
  });
}

function probeSnapshot(page: Page): Promise<NavigationProbe> {
  return page.evaluate(() =>
    structuredClone(
      (window as unknown as { __navigationProbe: NavigationProbe }).__navigationProbe
    )
  );
}

async function requestAndSettle(
  page: Page,
  predicate: (response: Response) => boolean,
  action: () => Promise<unknown>
): Promise<{ before: NavigationProbe; response: Response }> {
  const before = await probeSnapshot(page);
  const responsePromise = page.waitForResponse(predicate, { timeout: 15_000 });
  await action();
  const response = await responsePromise;
  await expect
    .poll(async () => (await probeSnapshot(page)).afterRequests, { timeout: 5_000 })
    .toBe(before.afterRequests + 1);
  if (response.status() === 200) {
    await expect
      .poll(async () => (await probeSnapshot(page)).afterSwaps, { timeout: 5_000 })
      .toBe(before.afterSwaps + 1);
  }
  return { before, response };
}

function isUserTableResponse(response: Response): boolean {
  return (
    response.url().includes('/_table') &&
    response.request().headers()['ro-no-push'] !== 'true'
  );
}

function expectNoMotion(before: NavigationProbe, after: NavigationProbe): void {
  expect(after.nativeStarts).toBe(before.nativeStarts);
  expect(after.beforeTransitions).toBe(before.beforeTransitions);
}

async function twoFrames(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      })
  );
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop navigation and detail surfaces');
  await control('/__control/reset');
});

test('body and list navigation remain plain HTMX swaps with native motion disabled', async ({
  page,
}) => {
  await installNavigationProbe(page);
  await page.goto(PODS);
  expect(
    await page.evaluate(
      () =>
        (window as unknown as { htmx: { config: { globalViewTransitions: boolean } } }).htmx
          .config.globalViewTransitions
    )
  ).toBe(false);

  const navigation = await requestAndSettle(
    page,
    (response) =>
      new URL(response.url()).pathname === SERVICES &&
      response.request().headers()['hx-request'] === 'true',
    () => page.locator('.ro-sidebar a', { hasText: 'Services' }).click()
  );
  expect(navigation.response.status()).toBe(200);
  await expect(page).toHaveURL(new RegExp(`${SERVICES}$`));
  expectNoMotion(navigation.before, await probeSnapshot(page));

  const sort = await requestAndSettle(page, isUserTableResponse, () =>
    page.locator('thead th a', { hasText: 'Name' }).first().click()
  );
  expect(sort.response.status()).toBe(200);
  await expect(page).toHaveURL(/\?sort=Name$/u);
  expectNoMotion(sort.before, await probeSnapshot(page));
});

test('an exact active sidebar click is a no-op while a real query reset still navigates', async ({
  page,
}) => {
  await installNavigationProbe(page);
  await page.goto(NAMESPACES);
  const before = await probeSnapshot(page);
  const historyLength = await page.evaluate(() => window.history.length);
  const main = await page.locator('main').elementHandle();
  expect(main).not.toBeNull();
  const boostedRequests: string[] = [];
  page.on('request', (request) => {
    if (request.headers()['hx-request'] === 'true') boostedRequests.push(request.url());
  });

  await page.locator('.ro-sidebar a.is-active', { hasText: 'Namespaces' }).click();
  await twoFrames(page);

  expect(boostedRequests).toEqual([]);
  expect(page.url()).toBe(new URL(NAMESPACES, page.url()).href);
  expect(await page.evaluate(() => window.history.length)).toBe(historyLength);
  expect(await main?.evaluate((element) => element.isConnected)).toBe(true);
  expect(await probeSnapshot(page)).toMatchObject(before);

  await page.goto(`${NAMESPACES}?sort=Name`);
  boostedRequests.length = 0;
  const reset = await requestAndSettle(
    page,
    (response) =>
      new URL(response.url()).pathname === NAMESPACES &&
      new URL(response.url()).search === '' &&
      response.request().headers()['hx-request'] === 'true',
    () => page.locator('.ro-sidebar a.is-active', { hasText: 'Namespaces' }).click()
  );
  expect(reset.response.status()).toBe(200);
  expect(boostedRequests).toHaveLength(1);
  expect(page.url()).toBe(new URL(NAMESPACES, page.url()).href);
  expectNoMotion(reset.before, await probeSnapshot(page));
});

test('repeated Default clicks preserve Containers and YAML is complete before first paint', async ({
  page,
}) => {
  await installNavigationProbe(page);
  await page.goto(POD);
  const before = await probeSnapshot(page);
  const historyLength = await page.evaluate(() => window.history.length);
  const containers = await page.locator('.ro-containers').elementHandle();
  expect(containers).not.toBeNull();
  const boostedRequests: string[] = [];
  page.on('request', (request) => {
    if (request.headers()['hx-request'] === 'true') boostedRequests.push(request.url());
  });

  const defaultTab = page.locator('.ro-tabs a.is-active', { hasText: 'Default' });
  await defaultTab.click();
  await defaultTab.click();
  await twoFrames(page);

  expect(boostedRequests).toEqual([]);
  expect(page.url()).toBe(new URL(POD, page.url()).href);
  expect(await page.evaluate(() => window.history.length)).toBe(historyLength);
  expect(await containers?.evaluate((element) => element.isConnected)).toBe(true);
  expect(await probeSnapshot(page)).toMatchObject(before);

  await page.evaluate(() => {
    const probe = (window as unknown as { __navigationProbe: NavigationProbe })
      .__navigationProbe;
    const onSwap = (event: Event): void => {
      if (event.target !== document.body) return;
      document.removeEventListener('htmx:afterSwap', onSwap);
      requestAnimationFrame(() => {
        const line = document.querySelector('#yaml-line-3');
        const token = line?.querySelector(':scope > span:not(.ro-fold-note)');
        const toggle = line?.querySelector(':scope > .ro-fold-toggle');
        probe.firstPaint = {
          folds: document.querySelectorAll('.ro-fold-toggle').length,
          directToggle: toggle !== null,
          controlPosition: toggle ? getComputedStyle(toggle).position : null,
          tokenX: token?.getBoundingClientRect().x ?? null,
        };
      });
    };
    document.addEventListener('htmx:afterSwap', onSwap);
  });

  boostedRequests.length = 0;
  const yaml = await requestAndSettle(
    page,
    (response) =>
      new URL(response.url()).pathname === POD &&
      new URL(response.url()).search === '?view=yaml' &&
      response.request().headers()['hx-request'] === 'true',
    () => page.locator('.ro-tabs a', { hasText: 'YAML' }).click()
  );
  expect(yaml.response.status()).toBe(200);
  await expect.poll(async () => (await probeSnapshot(page)).firstPaint).not.toBeNull();
  const firstPaint = (await probeSnapshot(page)).firstPaint;
  expect(firstPaint).toMatchObject({
    directToggle: true,
    controlPosition: 'absolute',
  });
  expect(firstPaint?.folds).toBeGreaterThan(0);
  expect(firstPaint?.tokenX).not.toBeNull();
  const settledTokenX = await page
    .locator('#yaml-line-3 > span:not(.ro-fold-note)')
    .first()
    .evaluate((element) => element.getBoundingClientRect().x);
  expect(firstPaint?.tokenX).toBeCloseTo(settledTokenX, 2);
  expectNoMotion(yaml.before, await probeSnapshot(page));

  const restored = await requestAndSettle(
    page,
    (response) =>
      new URL(response.url()).pathname === POD &&
      new URL(response.url()).search === '' &&
      response.request().headers()['hx-request'] === 'true',
    () => page.locator('.ro-tabs a', { hasText: 'Default' }).click()
  );
  expect(restored.response.status()).toBe(200);
  await expect(page.locator('.ro-containers')).toBeVisible();
  expectNoMotion(restored.before, await probeSnapshot(page));
});

test('cache-hit Back restores content without creating browser motion', async ({ page }) => {
  await installNavigationProbe(page);
  await page.goto(PODS);
  await requestAndSettle(
    page,
    (response) => new URL(response.url()).pathname === SERVICES,
    () => page.locator('.ro-sidebar a', { hasText: 'Services' }).click()
  );

  const beforeBack = await probeSnapshot(page);
  await page.goBack();
  await expect.poll(() => new URL(page.url()).pathname).toBe(PODS);
  await expect(page.locator('tr[data-key="e2e/default/nginx"]')).toBeVisible();
  await expect
    .poll(async () => (await probeSnapshot(page)).historyCacheHits, { timeout: 5_000 })
    .toBe(beforeBack.historyCacheHits + 1);
  expectNoMotion(beforeBack, await probeSnapshot(page));
});
