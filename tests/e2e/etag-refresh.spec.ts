import { expect, test, type Page, type Request, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Application-managed conditional refresh, end to end:
//
//   - the full page deliberately does not seed the `_table` validator; the
//     first container-owned refresh does (200 + weak ETag), and an unchanged
//     second refresh is a real 304 that performs no morph;
//   - a changed Kubernetes LIST rotates the validator (200), after which the
//     next unchanged refresh is 304 again;
//   - user sort/filter gestures remain unconditional and receive the canonical
//     HX-Push-Url -- only container-owned RO-No-Push refreshes reuse an ETag;
//   - an upstream 500 is never hidden by a previously-good validator, while a
//     subsequent unchanged 304 is a successful stale recovery without a swap;
//   - history never restores a windowed tbody slice into a bodyless 304: its
//     validator is cleared before one unconditional full-model rebuild.
//
// The tests call window.requestListRefresh(), the production tick/Retry entry
// point. They therefore exercise htmx:configRequest and its real request
// headers without paying the 5s polling interval. A network response arrives
// before HTMX necessarily finishes its lifecycle, so every action also waits
// for the list's htmx:afterRequest event through the page-side probe below.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const NGINX_KEY = 'e2e/default/nginx';
const BIG_PODS = '/clusters/e2e/namespaces/big/pods';
const BIG_SERVICES = '/clusters/e2e/namespaces/big/services';
const BIG_LAST_KEY = 'e2e/big/big-pod-0600';

interface LifecycleCounts {
  afterRequests: number;
  afterSwaps: number;
  responseErrors: number;
}

interface HistoryCounts {
  restores: number;
  listAfterSettles: number;
}

async function control(path: string): Promise<void> {
  const response = await fetch(controlURL + path);
  if (!response.ok) {
    throw new Error(`control ${path}: ${response.status} ${await response.text()}`);
  }
}

async function scriptEvents(events: object[]): Promise<void> {
  const response = await fetch(`${controlURL}/__control/watch-script`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ events }),
  });
  if (!response.ok) {
    throw new Error(`watch-script: ${response.status} ${await response.text()}`);
  }
}

function isTickResponse(response: Response): boolean {
  return (
    response.url().includes('/_table') &&
    response.request().headers()['ro-no-push'] === 'true'
  );
}

function isUserTableResponse(response: Response): boolean {
  const headers = response.request().headers();
  return response.url().includes('/_table') && headers['ro-no-push'] !== 'true';
}

function isBigPodsTickRequest(request: Request): boolean {
  return (
    new URL(request.url()).pathname === `${BIG_PODS}/_table` &&
    request.headers()['ro-no-push'] === 'true'
  );
}

function isBigPodsTickResponse(response: Response): boolean {
  return isBigPodsTickRequest(response.request());
}

function weakETag(response: Response): string {
  const etag = response.headers().etag;
  expect(etag).toMatch(/^W\/"[^"\r\n]+"$/);
  return etag as string;
}

function expectConditionalMetadata(response: Response, etag: string): void {
  expect(response.headers().etag).toBe(etag);
  expect(response.headers()['cache-control']).toBe('private, no-store');
  const vary = (response.headers().vary ?? '')
    .split(',')
    .map((token) => token.trim().toLowerCase())
    .filter(Boolean);
  expect(vary).toEqual(['accept-encoding']);
}

async function installLifecycleProbe(page: Page): Promise<void> {
  await page.evaluate(() => {
    type Probe = LifecycleCounts & { rememberedRow: Element | null };
    const probe: Probe = {
      afterRequests: 0,
      afterSwaps: 0,
      responseErrors: 0,
      rememberedRow: null,
    };
    (window as unknown as { __etagProbe: Probe }).__etagProbe = probe;

    const isListEvent = (event: Event): boolean => {
      const detail = Object((event as CustomEvent).detail) as {
        elt?: { id?: unknown };
        target?: { id?: unknown };
      };
      return (
        detail.elt?.id === 'resource-list-content' ||
        detail.target?.id === 'resource-list-content'
      );
    };
    document.addEventListener('htmx:afterRequest', (event) => {
      if (isListEvent(event)) probe.afterRequests += 1;
    });
    document.addEventListener('htmx:afterSwap', (event) => {
      if (isListEvent(event)) probe.afterSwaps += 1;
    });
    document.addEventListener('htmx:responseError', (event) => {
      if (isListEvent(event)) probe.responseErrors += 1;
    });
  });
}

async function installHistoryProbe(page: Page): Promise<void> {
  await page.evaluate(() => {
    const probe: HistoryCounts = { restores: 0, listAfterSettles: 0 };
    (window as unknown as { __etagHistoryProbe: HistoryCounts }).__etagHistoryProbe =
      probe;

    document.addEventListener('htmx:historyRestore', () => {
      probe.restores += 1;
    });
    document.addEventListener('htmx:afterSettle', (event) => {
      const detail = Object((event as CustomEvent).detail) as {
        elt?: { id?: unknown };
        target?: { id?: unknown };
      };
      if (
        detail.elt?.id === 'resource-list-content' ||
        detail.target?.id === 'resource-list-content'
      ) {
        probe.listAfterSettles += 1;
      }
    });
  });
}

function lifecycleCounts(page: Page): Promise<LifecycleCounts> {
  return page.evaluate(() => {
    const probe = (
      window as unknown as {
        __etagProbe: LifecycleCounts;
      }
    ).__etagProbe;
    return {
      afterRequests: probe.afterRequests,
      afterSwaps: probe.afterSwaps,
      responseErrors: probe.responseErrors,
    };
  });
}

function historyCounts(page: Page): Promise<HistoryCounts> {
  return page.evaluate(() => {
    const probe = (
      window as unknown as {
        __etagHistoryProbe: HistoryCounts;
      }
    ).__etagHistoryProbe;
    return {
      restores: probe.restores,
      listAfterSettles: probe.listAfterSettles,
    };
  });
}

async function rememberNginxRow(page: Page): Promise<void> {
  await page.evaluate((key) => {
    const probe = (
      window as unknown as {
        __etagProbe: LifecycleCounts & { rememberedRow: Element | null };
      }
    ).__etagProbe;
    probe.rememberedRow = document.querySelector(`tr[data-key="${key}"]`);
  }, NGINX_KEY);
}

function rememberedNginxRowIsCurrent(page: Page): Promise<boolean> {
  return page.evaluate((key) => {
    const probe = (
      window as unknown as {
        __etagProbe: LifecycleCounts & { rememberedRow: Element | null };
      }
    ).__etagProbe;
    return probe.rememberedRow === document.querySelector(`tr[data-key="${key}"]`);
  }, NGINX_KEY);
}

async function requestAndSettle(
  page: Page,
  predicate: (response: Response) => boolean,
  action: () => Promise<unknown>
): Promise<Response> {
  const before = await lifecycleCounts(page);
  const responsePromise = page.waitForResponse(predicate, { timeout: 15_000 });
  await action();
  const response = await responsePromise;
  await expect
    .poll(async () => (await lifecycleCounts(page)).afterRequests, { timeout: 5_000 })
    .toBe(before.afterRequests + 1);
  // A 200 list response is not settled until its one morph has reached
  // afterSwap; without this barrier the next request can race the previous
  // response's DOM/ETag repair.
  if (response.status() === 200) {
    await expect
      .poll(async () => (await lifecycleCounts(page)).afterSwaps, { timeout: 5_000 })
      .toBe(before.afterSwaps + 1);
  }
  return response;
}

function refreshNow(page: Page): Promise<Response> {
  return requestAndSettle(page, isTickResponse, () =>
    page.evaluate(() => window.requestListRefresh())
  );
}

function canonicalPathFromPartial(response: Response): string {
  const url = new URL(response.url());
  expect(url.pathname.endsWith('/_table')).toBe(true);
  return `${url.pathname.slice(0, -'/_table'.length)}${url.search}`;
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'conditional refresh asserts the desktop table, sort, and chips-editor surface'
  );
  await control('/__control/reset');
});

test('unchanged refresh is 304 without a morph; mutation rotates the tag; user sort and filter stay unconditional', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  await installLifecycleProbe(page);

  // A full-page response does not seed the `_table` validator. The first
  // production refresh is unconditional and stores the weak ETag from its 200.
  const seeded = await refreshNow(page);
  expect(seeded.status()).toBe(200);
  expect(seeded.request().headers()['if-none-match']).toBeUndefined();
  const tag1 = weakETag(seeded);
  expectConditionalMetadata(seeded, tag1);
  await expect.poll(async () => (await lifecycleCounts(page)).afterSwaps).toBe(1);
  await rememberNginxRow(page);

  // The same exact partial URL now carries tag1. A 304 settles successfully,
  // but it must not enter the afterSwap repair/virtualizer/Live pipeline.
  const beforeFirst304 = await lifecycleCounts(page);
  const unchanged = await refreshNow(page);
  expect(unchanged.status()).toBe(304);
  expect(unchanged.request().headers()['if-none-match']).toBe(tag1);
  expectConditionalMetadata(unchanged, tag1);
  expect(unchanged.headers()['content-length']).toBeUndefined();
  expect(unchanged.headers()['content-encoding']).toBeUndefined();
  expect(await lifecycleCounts(page)).toEqual({
    ...beforeFirst304,
    afterRequests: beforeFirst304.afterRequests + 1,
  });
  expect(await rememberedNginxRowIsCurrent(page)).toBe(true);

  // The control mutation is synchronous: the next LIST sees the new status.
  // The request still presents tag1, but changed semantics force a 200 + tag2.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'nginx',
          namespace: 'default',
          creationTimestamp: '2024-01-01T00:00:00Z',
          uid: '00000000-0000-0000-0000-000000000001',
        },
        status: { phase: 'Running' },
      },
      cells: ['nginx', '0/1', 'CrashLoopBackOff', '3', '10m'],
    },
  ]);
  const changed = await refreshNow(page);
  expect(changed.status()).toBe(200);
  expect(changed.request().headers()['if-none-match']).toBe(tag1);
  const tag2 = weakETag(changed);
  expect(tag2).not.toBe(tag1);
  expectConditionalMetadata(changed, tag2);
  await expect(
    page.locator(`tr[data-key="${NGINX_KEY}"] td:has(span.cell-status)`)
  ).toContainText('CrashLoopBackOff');
  expect((await lifecycleCounts(page)).afterSwaps).toBe(beforeFirst304.afterSwaps + 1);
  // Idiomorph preserves the keyed row through the changed 200 as well.
  expect(await rememberedNginxRowIsCurrent(page)).toBe(true);

  const beforeSecond304 = await lifecycleCounts(page);
  const stableAgain = await refreshNow(page);
  expect(stableAgain.status()).toBe(304);
  expect(stableAgain.request().headers()['if-none-match']).toBe(tag2);
  expectConditionalMetadata(stableAgain, tag2);
  expect(await lifecycleCounts(page)).toEqual({
    ...beforeSecond304,
    afterRequests: beforeSecond304.afterRequests + 1,
  });
  expect(await rememberedNginxRowIsCurrent(page)).toBe(true);

  // A real sort gesture is user-owned: no programmatic marker and no
  // conditional header. Its response is 200 and pushes the canonical page URL.
  const sorted = await requestAndSettle(page, isUserTableResponse, () =>
    page.locator('thead th a', { hasText: 'Name' }).first().click()
  );
  expect(sorted.status()).toBe(200);
  expect(sorted.request().headers()['ro-no-push']).toBeUndefined();
  expect(sorted.request().headers()['if-none-match']).toBeUndefined();
  weakETag(sorted);
  const sortedCanonical = canonicalPathFromPartial(sorted);
  expect(sorted.headers()['hx-push-url']).toBe(sortedCanonical);
  await expect.poll(() => new URL(page.url()).pathname + new URL(page.url()).search).toBe(
    sortedCanonical
  );

  // The chips editor is a second, independently-sourced user request. It must
  // also stay unconditional and push its filter-bearing canonical URL.
  const filterInput = page.locator('#ro-filter-input');
  await filterInput.click();
  await filterInput.pressSequentially('status:Running');
  const filtered = await requestAndSettle(page, isUserTableResponse, () =>
    filterInput.press('Enter')
  );
  expect(filtered.status()).toBe(200);
  expect(filtered.request().headers()['ro-no-push']).toBeUndefined();
  expect(filtered.request().headers()['if-none-match']).toBeUndefined();
  weakETag(filtered);
  const filteredCanonical = canonicalPathFromPartial(filtered);
  expect(filtered.headers()['hx-push-url']).toBe(filteredCanonical);
  await expect.poll(() => new URL(page.url()).pathname + new URL(page.url()).search).toBe(
    filteredCanonical
  );
  await expect(rowNames(page)).toHaveText(['my-app']);
});

test('a seeded validator never masks a 500; Retry now recovers with an unchanged 304 and no swap', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  await installLifecycleProbe(page);

  const seeded = await refreshNow(page);
  expect(seeded.status()).toBe(200);
  const tag = weakETag(seeded);
  expectConditionalMetadata(seeded, tag);
  await rememberNginxRow(page);
  const afterSeed = await lifecycleCounts(page);

  await control('/__control/fail-lists?mode=500');
  const failed = await refreshNow(page);
  expect(failed.status()).toBe(500);
  expect(failed.request().headers()['if-none-match']).toBe(tag);
  await expect(page.locator('.ro-stale-banner')).toBeVisible();
  await expect(page.locator('#resource-list-content')).toHaveClass(/ro-stale/);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  expect(await rememberedNginxRowIsCurrent(page)).toBe(true);
  expect(await lifecycleCounts(page)).toEqual({
    afterRequests: afterSeed.afterRequests + 1,
    afterSwaps: afterSeed.afterSwaps,
    responseErrors: afterSeed.responseErrors + 1,
  });

  // The fake LIST is byte/semantically unchanged once the forced failure is
  // disabled. Retry now goes through the same container-owned refresh path,
  // presents the retained tag, and treats 304 as recovery without a morph.
  await control('/__control/fail-lists?mode=off');
  const beforeRecovery = await lifecycleCounts(page);
  const recovered = await requestAndSettle(page, isTickResponse, () =>
    page.locator('.ro-stale-banner .ro-stale-retry').click()
  );
  expect(recovered.status()).toBe(304);
  expect(recovered.request().headers()['if-none-match']).toBe(tag);
  expectConditionalMetadata(recovered, tag);
  expect(recovered.headers()['content-length']).toBeUndefined();
  expect(recovered.headers()['content-encoding']).toBeUndefined();
  await expect(page.locator('.ro-stale-banner')).toBeHidden();
  await expect(page.locator('#resource-list-content')).not.toHaveClass(/ro-stale/);
  await expect(page.locator('#ro-toasts .ro-toast')).toHaveText('Refresh resumed');
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  expect(await rememberedNginxRowIsCurrent(page)).toBe(true);
  expect(await lifecycleCounts(page)).toEqual({
    ...beforeRecovery,
    afterRequests: beforeRecovery.afterRequests + 1,
  });
});

test('history restores a windowed list with one unconditional 200 full-model rebuild', async ({
  page,
}) => {
  await page.goto(BIG_PODS);
  const identityRows = page.locator('#resource-list-content tbody tr[data-key]');
  await expect(page.locator('tr[data-key="e2e/big/big-pod-0001"]')).toBeVisible();
  expect(await identityRows.count()).toBeGreaterThan(10);
  expect(await identityRows.count()).toBeLessThan(100);
  await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
  await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
  await expect(page.locator('.ro-count')).toHaveText('600');
  await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');
  await installLifecycleProbe(page);

  // Seed the exact `_table` representation while the body contains only the
  // virtualizer's current row window plus spacers. That validator and sliced
  // tbody are what HTMX will serialize into its history cache below.
  const seeded = await refreshNow(page);
  expect(seeded.status()).toBe(200);
  expect(seeded.request().headers()['if-none-match']).toBeUndefined();
  const tag = weakETag(seeded);
  expectConditionalMetadata(seeded, tag);
  const tablePath = `${new URL(seeded.url()).pathname}${new URL(seeded.url()).search}`;
  await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
  await expect
    .poll(() =>
      page.evaluate(() => {
        const content = document.getElementById('resource-list-content') as HTMLElement;
        return { etag: content.dataset.roEtag, path: content.dataset.roEtagPath };
      })
    )
    .toEqual({ etag: tag, path: tablePath });
  await installHistoryProbe(page);

  const servicesResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_SERVICES &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Services' }).click();
  const servicesResponse = await servicesResponsePromise;
  expect(servicesResponse.status()).toBe(200);
  await page.waitForURL(`**${BIG_SERVICES}`);
  await expect(
    page.getByRole('navigation', { name: 'breadcrumbs' }).getByText('services', {
      exact: true,
    })
  ).toBeVisible();
  await expect(page.locator('.ro-table-wrap.ro-windowed')).toHaveCount(0);

  const beforeHistory = await historyCounts(page);
  const rebuildRequests: Request[] = [];
  const captureRebuild = (request: Request): void => {
    if (isBigPodsTickRequest(request)) rebuildRequests.push(request);
  };
  page.on('request', captureRebuild);
  try {
    // A real cache-hit popstate emits htmx:historyRestore. Its cached tbody has
    // spacers but no 600-row model, so virtualizeInit must synchronously discard
    // the cached validator before issuing exactly one container-owned rebuild.
    const rebuilt = await requestAndSettle(page, isBigPodsTickResponse, () =>
      page.goBack()
    );
    expect(rebuilt.status()).toBe(200);
    expect(rebuilt.request().headers()['ro-no-push']).toBe('true');
    expect(rebuilt.request().headers()['if-none-match']).toBeUndefined();
    const rebuiltTag = weakETag(rebuilt);
    expect(rebuiltTag).toBe(tag);
    expectConditionalMetadata(rebuilt, rebuiltTag);

    await expect
      .poll(async () => (await historyCounts(page)).restores, { timeout: 5_000 })
      .toBe(beforeHistory.restores + 1);
    // htmx:load for the 200 swap runs before afterSettle. Reaching this event
    // is therefore an event-loop barrier for both the restore-time load burst
    // and the rebuilt fragment's own init pass; neither may start a duplicate.
    await expect
      .poll(async () => (await historyCounts(page)).listAfterSettles, {
        timeout: 5_000,
      })
      .toBe(beforeHistory.listAfterSettles + 1);
    expect(rebuildRequests).toHaveLength(1);
    expect(rebuildRequests[0]).toBe(rebuilt.request());
  } finally {
    page.off('request', captureRebuild);
  }

  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
  await expect(page.locator('.ro-table-wrap.ro-windowed')).toBeVisible();
  await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
  await expect(page.locator('.ro-count')).toHaveText('600');
  await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              roVirtual: { renderedBounds(): { total: number } };
            }
          ).roVirtual.renderedBounds().total
      )
    )
    .toBe(600);

  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await expect(page.locator(`tr[data-key="${BIG_LAST_KEY}"]`)).toBeVisible();
});
