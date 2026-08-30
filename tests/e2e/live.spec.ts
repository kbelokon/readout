import { test, expect, type Page, type Request } from "@playwright/test";
import { controlURL } from "./playwright.config";
import { resetFixture } from "./hub";

// Live mode, made deterministic by the fakeapi control surface (watch-script
// with delayMs, watch-401, the openWatches snapshot) plus Playwright's page
// clock. Live is the ONLY automatic update path now -- there is no polling to
// fall back to -- so what a dropped stream does is as load-bearing as what an
// open one delivers:
//
//   - the topbar toggle opens the `_stream` SSE fetch (with a client-minted
//     generation header), presses aria-pressed, and a scripted pod status
//     change lands as a PUSH that flashes exactly the changed cell through the
//     semantic delta reducer; the choice persists via the ro_prefs cookie;
//   - a filter-chip request synchronously aborts the OLD stream before the
//     request leaves the browser, stays suspended while canonical state
//     changes, then reopens with a FRESH generation carrying the f= query;
//   - a sort moves the stream to the new URL, while a FAILED one reopens
//     against the PREVIOUS one: the browser follows what actually rendered,
//     never what was attempted;
//   - `/__control/watch-401` + a scripted EOF make readout's re-watch hit a 401
//     -> the server emits a `ro-live` terminal (reason auth) -> the banner
//     swaps to its terminal Unavailable copy whose action is Reload, and NO
//     retry is ever armed (nothing polls in its place);
//   - a 429 admission reject waits out the server's Retry-After rather than
//     the client's own ladder;
//   - a drop marks the projection last-known IMMEDIATELY but hides the dim and
//     the banner for a three second grace, so a pod rollout does not flash a
//     warning; the first FAILED reconnect ends that grace early;
//   - document.hidden closes a riding stream and visibility return reopens it
//     exactly once;
//   - a push into the 600-row windowed fixture keeps the window: no duplicate
//     rows, two spacers, stable mid-list scroll, the changed cell flashes;
//   - pages `_stream` does not serve (multi-type, multi-cluster, detail, and a
//     kind whose verbs lack `watch`) render Refresh and NO Live toggle at all.
//
// NOTE on openWatchCount: an upstream watch is owned by the pod-local WatchHub,
// not by one browser (see hub.ts). A source is retained for 30 seconds after
// its last subscriber leaves, so "the client stopped streaming" can NOT be
// observed as "the upstream watch closed". Client-side disconnection is
// asserted through the transport's own state (roLive.stats) and through the
// absence of new `_stream` requests; openWatchCount is read only in the
// positive direction, and resetFixture is what keeps a retained source from
// bleeding its pre-reset table into the next spec.

const PODS = "/clusters/e2e/namespaces/default/pods";
const PODS_LIST_PATH = "/api/v1/namespaces/default/pods";
const BIG_PODS = "/clusters/e2e/namespaces/big/pods";
const BIG_PODS_LIST_PATH = "/api/v1/namespaces/big/pods";
const NGINX_POD = "/clusters/e2e/namespaces/default/pods/nginx";
// The metrics pseudo-type resolves with get/list verbs and no watch
// (kube.metricsResourceType), so here it is the WATCH gate -- not the scope
// gate -- that withholds the toggle. `_stream` answers this URL 204.
const METRICS_PODS =
  "/clusters/e2e/namespaces/default/pods?apiVersion=metrics.k8s.io%2Fv1beta1";

const LIVE_TOGGLE = '[data-ro-action="toggle-live"]';
const REFRESH_NOW = '[data-ro-action="refresh-now"]';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

async function scriptEvents(events: object[]): Promise<void> {
  const res = await fetch(`${controlURL}/__control/watch-script`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ events }),
  });
  if (!res.ok) {
    throw new Error(`watch-script: ${res.status} ${await res.text()}`);
  }
}

// The fakeapi hub snapshot names every open ?watch=true connection -- the
// server-side truth about whether SOMETHING on the pod is watching this key.
async function openWatchCount(): Promise<number> {
  const res = await fetch(`${controlURL}/__control/watch-script`);
  if (!res.ok) {
    throw new Error(`watch snapshot: ${res.status}`);
  }
  const body = (await res.json()) as { openWatches?: string[] };
  return (body.openWatches ?? []).length;
}

async function appliedScriptEventCount(): Promise<number> {
  const res = await fetch(`${controlURL}/__control/watch-script`);
  if (!res.ok) {
    throw new Error(`watch snapshot: ${res.status}`);
  }
  const body = (await res.json()) as { events?: Array<{ applied?: boolean }> };
  return (body.events ?? []).filter((event) => event.applied === true).length;
}

interface LiveStats {
  state: string;
  connections: number;
  reconnects: number;
  inFlightRequests: number;
}

function liveStats(page: Page): Promise<LiveStats> {
  return page.evaluate(() => {
    const stats = (
      window as unknown as { roLive: { stats(): LiveStats } }
    ).roLive.stats();
    return {
      state: stats.state,
      connections: stats.connections,
      reconnects: stats.reconnects,
      inFlightRequests: stats.inFlightRequests,
    };
  });
}

function liveState(page: Page): Promise<string> {
  return liveStats(page).then((stats) => stats.state);
}

function isStreamRequest(url: string): boolean {
  return new URL(url).pathname.endsWith("/_stream");
}

function streamGeneration(request: Request): string {
  return request.headers()["ro-live-generation"] ?? "";
}

// streamLog is the append-only record of every `_stream` request the page
// issued. Reconnect assertions are about COUNTS and URLs, so the log is the
// primary oracle for both "it retried" and "it did not".
function streamLog(page: Page): string[] {
  const urls: string[] = [];
  page.on("request", (request) => {
    if (isStreamRequest(request.url())) urls.push(request.url());
  });
  return urls;
}

function rowNames(page: Page) {
  return page.locator(
    "#resource-list-content table.ro-table tbody td.cell-name",
  );
}

function podRow(page: Page, n: number) {
  return page.locator(
    `tr[data-key="e2e/big/big-pod-${String(n).padStart(4, "0")}"]`,
  );
}

// enableLive clicks the topbar Live toggle and waits until a full snapshot has
// been committed -- `open` is the only state that means "the rows on screen
// came off this stream".
async function enableLive(page: Page): Promise<Request> {
  const opened = page.waitForRequest((r) => isStreamRequest(r.url()), {
    timeout: 10_000,
  });
  await page.locator(LIVE_TOGGLE).click();
  const request = await opened;
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");
  return request;
}

// Simulate tab visibility for the document.hidden taxonomy: readout.js reads
// document.hidden and listens for visibilitychange, both overridable.
async function setHidden(page: Page, hidden: boolean): Promise<void> {
  await page.evaluate((h) => {
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => h,
    });
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => (h ? "hidden" : "visible"),
    });
    document.dispatchEvent(new Event("visibilitychange"));
  }, hidden);
}

// The semantic half of staleness: the projection is last-known from the instant
// the stream drops, whatever the visual grace is still hiding.
function semanticallyStale(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      document.getElementById("resource-list-content")?.dataset.roStale ===
      "true",
  );
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== "desktop",
    "the Live chrome (topbar toggle, chips editor, windowing) is a desktop surface (below 760px the card layer replaces the table)",
  );
  await resetFixture();
});

test("the Live toggle opens the stream: a status change lands as a push and flashes, and the pick persists", async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(["nginx", "my-app"]);
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute(
    "aria-pressed",
    "false",
  );

  const request = await enableLive(page);
  expect(streamGeneration(request)).not.toBe(""); // the client minted a v2 generation
  expect(request.headers()["ro-live-version"]).toBe("2");
  // The server-side watch is riding (fakeapi hub truth).
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // A scripted change arrives as a PUSH: nothing is on a timer, so the cell
  // update proves the stream delivered it -- and the 1s budget pins it
  // SUB-SECOND. The changed STATUS cell flashes; the untouched NAME cell
  // does not.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: "MODIFIED",
      object: {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          name: "nginx",
          namespace: "default",
          uid: "00000000-0000-0000-0000-000000000001",
        },
        status: { phase: "Running" },
      },
      cells: ["nginx", "0/1", "CrashLoopBackOff", "3", "10m"],
    },
  ]);
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator("td:has(span.cell-status)")).toContainText(
    "CrashLoopBackOff",
    { timeout: 1_000 },
  );
  await expect(nginx.locator("td:has(span.cell-status)")).toHaveClass(
    /ro-cell-changed/,
  );
  await expect(nginx.locator("td.cell-name")).not.toHaveClass(
    /ro-cell-changed/,
  );

  // Persistence: a reload renders the toggle pressed at SSR from the ro_prefs
  // cookie and the stream reopens by itself (a fresh page init is a fresh
  // attempt).
  const reopened = page.waitForRequest((r) => isStreamRequest(r.url()), {
    timeout: 10_000,
  });
  await page.reload();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  await reopened;
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");
});

test("a filter request aborts the old generation before send, suspends, and reopens canonically", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  const gen1 = streamGeneration(await enableLive(page));
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
  const before = await liveStats(page);
  expect(streams).toHaveLength(1);

  // Hold the exact user `_table` request at the browser boundary. The route
  // starts only after htmx:beforeRequest has given Live synchronous ownership
  // and aborted the old connection, so the absence of a replacement `_stream`
  // below is causal evidence rather than a timing inference.
  let markTableStarted!: () => void;
  let releaseTable!: () => void;
  const tableStarted = new Promise<void>((resolve) => {
    markTableStarted = resolve;
  });
  const tableRelease = new Promise<void>((resolve) => {
    releaseTable = resolve;
  });
  // Self-retiring rather than `{ times: 1 }`: an exhausted times-limited route
  // is torn down at the instant its last request resolves, and the request Live
  // issues in that same turn can be dropped as interception unwinds. A handler
  // that stays registered and falls through never toggles interception.
  let tableHeld = false;
  await page.route("**/_table*", async (route) => {
    if (tableHeld) {
      await route.fallback();
      return;
    }
    tableHeld = true;
    markTableStarted();
    await tableRelease;
    await route.continue();
  });

  // Begin a status:Running commit. No replacement generation may open while
  // this request owns the persistent list container.
  await page.locator("#ro-filter-input").click();
  await page.locator("#ro-filter-input").pressSequentially("status:Running");
  await page.locator("#ro-filter-input").press("Enter");
  await tableStarted;
  await expect.poll(() => liveStats(page)).toMatchObject({
    state: "suspended",
    connections: before.connections,
    inFlightRequests: 1,
  });
  expect(streams).toHaveLength(1);

  // Mutate canonical state while both the old stream and the held list request
  // are unable to update the page. The fakeapi applied flag proves the event
  // happened; the unchanged DOM plus the single recorded stream request prove
  // there was no stale generation delivery to discard at morph time.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: "MODIFIED",
      delayMs: 100,
      object: {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          name: "my-app",
          namespace: "default",
          uid: "00000000-0000-0000-0000-000000000002",
        },
        status: { phase: "Running" },
      },
      cells: ["my-app", "0/1", "CrashLoopBackOff", "7", "5m"],
    },
  ]);
  await expect.poll(appliedScriptEventCount, { timeout: 5_000 }).toBe(1);
  expect(streams).toHaveLength(1);
  await expect(
    page.locator('tr[data-key="e2e/default/my-app"] td:has(span.cell-status)'),
  ).toContainText("Running");

  // Release the request only after the mutation is canonical. Its 200 snapshot
  // filters my-app out, then the request barrier permits exactly the fresh,
  // query-coherent generation.
  const tableResponse = page.waitForResponse((r) =>
    r.url().includes("/_table"),
  );
  const secondStream = page.waitForRequest(
    (r) => isStreamRequest(r.url()) && streamGeneration(r) !== gen1,
    { timeout: 10_000 },
  );
  releaseTable();
  expect((await tableResponse).status()).toBe(200);
  const req2 = await secondStream;
  expect(req2.url()).toContain("f=status");
  await expect(rowNames(page)).toHaveText(["nginx"]);

  // The reopened stream is functional: a further nginx change (still
  // Running, so it stays in the filtered view) pushes through.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: "MODIFIED",
      object: {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          name: "nginx",
          namespace: "default",
          uid: "00000000-0000-0000-0000-000000000001",
        },
        status: { phase: "Running" },
      },
      cells: ["nginx", "1/1", "Running", "4", "10m"],
    },
  ]);
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator("td").nth(3)).toContainText("4", {
    timeout: 3_000,
  });
});

test("a sort moves the stream to the new URL; a FAILED one keeps the previous", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  const first = await enableLive(page);
  expect(new URL(first.url()).search).toBe("");

  // A SUCCESSFUL sort: the container swap commits the new URL, and the stream
  // that reopens after it describes exactly those rows.
  const sorted = page.waitForRequest(
    (r) =>
      isStreamRequest(r.url()) && new URL(r.url()).search === "?sort=Name",
    { timeout: 10_000 },
  );
  await page.locator("thead th a", { hasText: "Name" }).first().click();
  await sorted;
  await expect(page).toHaveURL(/\?sort=Name$/);
  await expect(rowNames(page)).toHaveText(["my-app", "nginx"]);
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");
  const settled = streams.length;

  // A FAILED sort: fail exactly the list request at the browser boundary, so
  // the server (and the hub source behind the stream) stays healthy. Same
  // self-retiring shape as above, for the same reason -- this settlement is
  // precisely what makes Live reopen.
  let failedOnce = false;
  await page.route("**/_table*", async (route) => {
    if (failedOnce) {
      await route.fallback();
      return;
    }
    failedOnce = true;
    await route.fulfill({ status: 500 });
  });
  const reopened = page.waitForRequest(
    (r) => isStreamRequest(r.url()) && streams.length > settled,
    { timeout: 10_000 },
  );
  await page.locator("thead th a", { hasText: "Name" }).first().click();
  const second = await reopened;

  // htmx does not swap on error, so neither the rendered projection nor the
  // URL moved. The replacement stream describes what is actually on screen --
  // a sort=Name:desc stream would be narrating rows nobody has.
  expect(new URL(second.url()).search).toBe("?sort=Name");
  await expect(page).toHaveURL(/\?sort=Name$/);
  await expect(rowNames(page)).toHaveText(["my-app", "nginx"]);
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");

  // Nothing was retried on the browser's own initiative -- the failed list
  // request is the user's to repeat. Exactly one replacement stream opened,
  // and its committed snapshot is what clears the failure's stale dim.
  expect(streams).toHaveLength(settled + 1);
  await expect(page.locator(".ro-stale-banner")).toBeHidden();
  await expect(page.locator("#resource-list-content")).not.toHaveClass(
    /ro-stale/,
  );
});

test("the auth terminal makes Live unavailable: the Reload banner, no retry, no further stream", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  await enableLive(page);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
  expect(streams).toHaveLength(1);

  // Arm the one-shot 401 for THIS collection route, then cleanly EOF the riding
  // watch: the hub source re-watches, hits the 401, and closes every subscriber
  // with reason "auth". The arm is path-scoped because `/api/v1/pods` is an
  // alias of this route -- it shares the list state, so the EOF wakes an
  // all-namespaces source too, and an unscoped 401 would be a coin flip
  // between the two reconnects.
  await page.clock.install();
  await control(`/__control/watch-401?path=${PODS_LIST_PATH}`);
  await scriptEvents([{ path: PODS_LIST_PATH, type: "EOF" }]);

  // Terminal: the banner shows the UNAVAILABLE copy, whose action is Reload
  // rather than Retry, and the recoverable copy with its countdown is hidden.
  const banner = page.locator(".ro-stale-banner");
  await expect(banner).toBeVisible({ timeout: 10_000 });
  await expect(banner.locator(".bn-body.ro-stale-unavailable")).toBeVisible();
  await expect(
    banner.locator(".bn-body.ro-stale-unavailable .bn-title"),
  ).toHaveText("Live updates unavailable — showing the last good data");
  await expect(banner.locator(".ro-stale-reload")).toBeVisible();
  await expect(banner.locator(".ro-stale-reload")).toHaveText("Reload");
  await expect(banner.locator(".ro-stale-retry")).toBeHidden();
  await expect(page.locator("#resource-list-content")).toHaveClass(/ro-stale/);
  expect(await liveState(page)).toBe("unavailable");

  // NOTHING replaces the stream: no retry is armed and no `_table` poll takes
  // its place. An hour of page-owned time proves the terminal is terminal.
  const tables: string[] = [];
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.endsWith("/_table"))
      tables.push(request.url());
  });
  await page.clock.fastForward(3_600_000);
  await page.waitForTimeout(250);
  expect(streams).toHaveLength(1);
  expect(tables).toEqual([]);

  // A tab hide/show cannot resurrect it either -- unavailable owns no wake-up.
  await setHidden(page, true);
  await setHidden(page, false);
  await page.waitForTimeout(250);
  expect(streams).toHaveLength(1);
  expect(await liveState(page)).toBe("unavailable");

  // The rows themselves survived: the terminal dims the last good data, it
  // never blanks it.
  await expect(rowNames(page)).toHaveText(["nginx", "my-app"]);
});

test("a 429 admission reject waits out the server's Retry-After, not the client ladder", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  await page.clock.install();

  // Every stream attempt is rejected with the admission status and a wait far
  // longer than any rung of the client's own ladder (which caps at 1s for the
  // first attempt), so the delay observed below can only be the header's.
  await page.route("**/_stream*", (route) =>
    route.fulfill({ status: 429, headers: { "Retry-After": "5" } }),
  );

  await page.locator(LIVE_TOGGLE).click();
  await expect
    .poll(() => streams.length, { timeout: 10_000 })
    .toBe(1);
  await expect
    .poll(() => liveState(page), { timeout: 10_000 })
    .toBe("reconnecting");

  // Four seconds in: still exactly one attempt. The ladder alone would have
  // retried three times over by now.
  await page.clock.fastForward(4_000);
  await page.waitForTimeout(250);
  expect(streams).toHaveLength(1);

  // Past the header's five seconds: exactly one retry.
  await page.clock.fastForward(1_500);
  await expect.poll(() => streams.length, { timeout: 10_000 }).toBe(2);
  expect(await liveStats(page)).toMatchObject({ state: "reconnecting" });

  // Turning Live off from a reconnecting state cancels the armed retry, issues
  // no request, and clears the warning surface.
  await page.locator(LIVE_TOGGLE).click();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute(
    "aria-pressed",
    "false",
  );
  expect(await liveState(page)).toBe("off");
  await expect(page.locator(".ro-stale-banner")).toBeHidden();
  await page.clock.fastForward(60_000);
  await page.waitForTimeout(250);
  expect(streams).toHaveLength(2);
});

test("a drop is stale at once but dims only after the three second grace", async ({
  page,
}) => {
  await page.goto(PODS);
  await enableLive(page);
  const content = page.locator("#resource-list-content");
  const banner = page.locator(".ro-stale-banner");
  await page.clock.install();

  // Going offline drops the stream through the transport's own lifecycle
  // listener. With the clock frozen, no timer can fire between the drop and
  // the assertions below, so the grace is observed, not raced.
  await page.context().setOffline(true);
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("offline");
  expect(await semanticallyStale(page)).toBe(true);
  await expect(content).not.toHaveClass(/ro-stale/);
  await expect(banner).toBeHidden();

  // Three seconds later the grace expires: the dim and the RECOVERABLE banner
  // appear together.
  await page.clock.fastForward(3_000);
  await expect(banner).toBeVisible();
  await expect(banner.locator(".bn-body.ro-stale-unavailable")).toBeHidden();
  await expect(banner.locator(".ro-stale-retry")).toBeVisible();
  await expect(content).toHaveClass(/ro-stale/);
  await expect(rowNames(page)).toHaveText(["nginx", "my-app"]);

  // Coming back online reconnects ONCE, and only the committed snapshot clears
  // both halves of the staleness.
  await page.context().setOffline(false);
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");
  await expect(banner).toBeHidden();
  await expect(content).not.toHaveClass(/ro-stale/);
  expect(await semanticallyStale(page)).toBe(false);
});

test("the first failed reconnect ends the stale grace early", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  await enableLive(page);
  const banner = page.locator(".ro-stale-banner");
  await page.clock.install();

  // Every reconnect from here fails at the browser boundary.
  await page.route("**/_stream*", (route) => route.abort());

  await page.context().setOffline(true);
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("offline");
  await expect(banner).toBeHidden();

  // `online` reconnects once; that attempt fails. The FIRST failure is still
  // inside the grace, which is deliberate -- one lost attempt is not yet
  // evidence of a real outage.
  await page.context().setOffline(false);
  await expect.poll(() => streams.length, { timeout: 10_000 }).toBeGreaterThan(1);
  await expect
    .poll(() => liveState(page), { timeout: 10_000 })
    .toBe("reconnecting");
  await expect(banner).toBeHidden();

  // Release ONE second of page-owned time -- a third of the grace -- which is
  // enough for rung 1 of the ladder. When that retry fails the drop is no
  // longer plausibly a rollout blip and the grace ends early: with the clock
  // frozen again here, the three second timer provably never ran, so the
  // banner below can only have come from the failed reconnect.
  await page.clock.fastForward(1_000);
  await expect(banner).toBeVisible();
  expect(streams.length).toBeGreaterThan(2);
  await expect(page.locator("#resource-list-content")).toHaveClass(/ro-stale/);
  await expect(rowNames(page)).toHaveText(["nginx", "my-app"]);
});

test("document.hidden closes the stream; visibility return reopens it exactly once", async ({
  page,
}) => {
  await page.goto(PODS);
  const streams = streamLog(page);
  const gen1 = streamGeneration(await enableLive(page));

  // Hide: the client aborts the stream fetch and parks. It owns its own
  // wake-up, so no retry is armed and no request is made.
  await setHidden(page, true);
  await expect.poll(() => liveState(page), { timeout: 5_000 }).toBe("hidden");
  expect(streams).toHaveLength(1);

  // Show: reopen under a FRESH generation -- exactly one reopen.
  const reopened = page.waitForRequest(
    (r) => isStreamRequest(r.url()) && streamGeneration(r) !== gen1,
    { timeout: 10_000 },
  );
  await setHidden(page, false);
  await reopened;
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe("open");
  expect(streams).toHaveLength(2);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
});

test("a Live push while windowed keeps the window: no duplicates, stable scroll, honest flash", async ({
  page,
}) => {
  await page.goto(BIG_PODS);
  await expect(podRow(page, 1)).toBeVisible();
  await enableLive(page);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // Push a change to an IN-WINDOW row (top of the list): the adopted cell
  // updates and flashes through the virtualizer's identity diff.
  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: "MODIFIED",
      object: {
        apiVersion: "v1",
        kind: "Pod",
        metadata: { name: "big-pod-0002", namespace: "big" },
      },
      cells: ["big-pod-0002", "0/1", "Error", "5", "10m"],
    },
  ]);
  await expect(podRow(page, 2).locator("td").nth(2)).toContainText("Error", {
    timeout: 3_000,
  });
  await expect(podRow(page, 2).locator("td").nth(2)).toHaveClass(
    /ro-cell-changed/,
  );
  await expect(podRow(page, 2).locator("td.cell-name")).not.toHaveClass(
    /ro-cell-changed/,
  );

  // The window survived the push: far fewer than 600 rows, NO duplicate
  // keys, both spacers, the true total -- 600 rows never rode the morph.
  const keys = await page.evaluate(() =>
    Array.from(
      document.querySelectorAll("#resource-list-content tbody tr[data-key]"),
      (tr) => tr.getAttribute("data-key"),
    ),
  );
  expect(keys.length).toBeGreaterThan(10);
  expect(keys.length).toBeLessThan(100);
  expect(new Set(keys).size).toBe(keys.length);
  await expect(page.locator("tr.ro-vspacer")).toHaveCount(2);
  await expect(page.locator(".ro-foundline")).toContainText("Found 600 rows");

  // Park mid-list and push a change into the CURRENT window: the scroll
  // position must hold exactly (the adoption pipeline restores it).
  await page.evaluate(() => window.scrollTo(0, 4000));
  await expect
    .poll(() => page.evaluate(() => window.scrollY), { timeout: 5_000 })
    .toBeGreaterThan(3500);
  const scrollBefore = await page.evaluate(() => window.scrollY);
  // The re-window rides a rAF-throttled scroll listener: wait until the
  // rendered slice actually moved before reading it, or the bounds still
  // describe the top-of-list window.
  await expect
    .poll(
      () =>
        page.evaluate(
          () =>
            (
              window as unknown as {
                roVirtual: { renderedBounds(): { start: number } };
              }
            ).roVirtual.renderedBounds().start,
        ),
      { timeout: 5_000 },
    )
    .toBeGreaterThan(50);
  const bounds = await page.evaluate(() =>
    (
      window as unknown as {
        roVirtual: { renderedBounds(): { start: number; end: number } };
      }
    ).roVirtual.renderedBounds(),
  );
  const inWindowPod = bounds.start + 6; // visible list index i is big-pod-(i+1)
  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: "MODIFIED",
      object: {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {
          name: `big-pod-${String(inWindowPod + 1).padStart(4, "0")}`,
          namespace: "big",
        },
      },
      cells: [
        `big-pod-${String(inWindowPod + 1).padStart(4, "0")}`,
        "0/1",
        "CrashLoopBackOff",
        "2",
        "10m",
      ],
    },
  ]);
  await expect(
    podRow(page, inWindowPod + 1)
      .locator("td")
      .nth(2),
  ).toContainText("CrashLoopBackOff", { timeout: 3_000 });
  expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);
  await expect(page.locator("tr.ro-vspacer")).toHaveCount(2);

  // The spacer offsets stayed exact: the last row is still reachable.
  await page.evaluate(() =>
    window.scrollTo(0, document.documentElement.scrollHeight),
  );
  await expect(podRow(page, 600)).toBeVisible();
});

test("pages the stream does not serve render Refresh and no Live toggle", async ({
  page,
}) => {
  // A rendered toggle is a promise `_stream` keeps, so every scope the endpoint
  // refuses must offer none at all -- while Refresh, which always applies,
  // renders everywhere. All three gates are covered: scope, page kind, and the
  // watch verb.
  for (const path of [
    "/clusters/e2e/namespaces/default/all", // multi-type plural
    "/clusters/e2e/namespaces/default/pods,services", // CSV multi-type
    "/clusters/_all/namespaces/default/pods", // multi-cluster union
    NGINX_POD, // a detail page has no list region to stream into
    METRICS_PODS, // a kind whose verbs do not include watch
  ]) {
    await page.goto(path);
    await expect(page.locator(LIVE_TOGGLE)).toHaveCount(0);
    await expect(page.locator(REFRESH_NOW)).toHaveCount(1);
  }

  // Single-type, single-cluster list of a watchable kind: the toggle is there.
  await page.goto(PODS);
  await expect(page.locator(LIVE_TOGGLE)).toHaveCount(1);
  await expect(page.locator(REFRESH_NOW)).toHaveCount(1);
});
