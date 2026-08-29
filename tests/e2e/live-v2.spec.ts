import {
  expect,
  test,
  type Page,
  type Request as PlaywrightRequest,
  type Response as PlaywrightResponse,
  type Route,
} from '@playwright/test';
import { controlURL } from './playwright.config';

// Negotiated Live v2, end to end.
//
// The page-side probe below wraps ONLY the application's `_stream` fetch. It
// retains the original request/response metadata and forwards the response
// through a frame-preserving ReadableStream. Complete SSE frames are therefore
// observable while the connection remains open; no response.body() call waits
// for EOF and no second observer steals bytes from the application. The same
// seam can corrupt one delta after it has been recorded or close the current
// response body, exercising decoder/resync and transport-EOF recovery without
// adding a fault API to the server.
//
// Every wait in this file has a protocol, network, lifecycle, watch, or DOM
// cause. There are deliberately no wall-clock sleeps.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const ALL_PODS = '/clusters/e2e/namespaces/_all/pods';
const ALL_PODS_LIST_PATH = '/api/v1/pods';
const EVENTS = '/clusters/e2e/namespaces/default/events';
const EVENTS_LIST_PATH = '/api/v1/namespaces/default/events';
const BIG_PODS = '/clusters/e2e/namespaces/big/pods';
const BIG_EVENTS = '/clusters/e2e/namespaces/big/events';
const BIG_PODS_LIST_PATH = '/api/v1/namespaces/big/pods';
const NGINX_KEY = 'e2e/default/nginx';
const BIG_VISIBLE_KEY = 'e2e/big/big-pod-0002';
const BIG_UNTOUCHED_KEY = 'e2e/big/big-pod-0001';
const BIG_OFFSCREEN_KEY = 'e2e/big/big-pod-0550';

type FaultKind = 'corrupt' | 'gap' | 'schema';

interface StreamRequestRecord {
  order: number;
  url: string;
  location: string;
  projectionRows: number | null;
  version: string | null;
  generation: string | null;
}

interface StreamResponseRecord {
  url: string;
  status: number;
  contentType: string | null;
  version: string | null;
  generation: string | null;
}

interface StreamFrameRecord {
  event: string | null;
  kind: string;
  v: number | null;
  g: string | null;
  seq: number | null;
  rev: string | null;
  schema: string | null;
  deltaBase: string | null;
  upsertKeys: string[];
  removeKeys: string[];
  orderLength: number | null;
  payloadBytes: number;
  rawBytes: number;
  snapshotHTMLBytes: number | null;
  faultApplied: FaultKind | null;
}

interface LifecycleProbe {
  afterRequests: number;
  afterSwaps: number;
  beforeTransitions: number;
  nativeStarts: number;
  events: {
    order: number;
    kind: string;
    location: string;
    requestURL: string | null;
  }[];
}

interface LiveTransportSnapshot {
  requests: StreamRequestRecord[];
  responses: StreamResponseRecord[];
  frames: StreamFrameRecord[];
  lifecycle: LifecycleProbe;
  fault: { kind: FaultKind | null; used: number };
  forcedEOFs: number;
  firstFrameBlocked: boolean;
  historyReloadBlocked: boolean;
  historyReloadRequests: number;
}

interface LiveTransportProbe extends LiveTransportSnapshot {
  closeCurrentStream(): Promise<void>;
  holdNextHistoryReload(): void;
  releaseHistoryReload(): void;
  setFault(kind: FaultKind): void;
  releaseFirstFrame(): void;
}

interface LiveStatsSnapshot {
  state: string;
  seq: number;
  connections: number;
  resyncs: number;
  fallbacks: number;
  v2Snapshots: number;
  deltas: number;
  invalidFrames: number;
  deltaBytes: number;
  inFlightRequests: number;
  resyncsInWindow: number;
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

async function openWatchCount(): Promise<number> {
  const response = await fetch(`${controlURL}/__control/watch-script`);
  if (!response.ok) throw new Error(`watch snapshot: ${response.status}`);
  const body = (await response.json()) as { openWatches?: string[] };
  return (body.openWatches ?? []).length;
}

function isStreamRequest(request: PlaywrightRequest): boolean {
  return new URL(request.url()).pathname.endsWith('/_stream');
}

function isContainerTableResponse(response: PlaywrightResponse): boolean {
  return (
    new URL(response.url()).pathname.endsWith('/_table') &&
    response.request().headers()['ro-no-push'] === 'true'
  );
}

function probeSnapshot(page: Page): Promise<LiveTransportSnapshot> {
  return page.evaluate(() => {
    const probe = (
      window as unknown as {
        __liveV2Probe: LiveTransportProbe;
      }
    ).__liveV2Probe;
    return structuredClone({
      requests: probe.requests,
      responses: probe.responses,
      frames: probe.frames,
      lifecycle: probe.lifecycle,
      fault: probe.fault,
      forcedEOFs: probe.forcedEOFs,
      firstFrameBlocked: probe.firstFrameBlocked,
      historyReloadBlocked: probe.historyReloadBlocked,
      historyReloadRequests: probe.historyReloadRequests,
    });
  });
}

function liveStats(page: Page): Promise<LiveStatsSnapshot> {
  return page.evaluate(
    () =>
      (
        window as unknown as {
          roLive: { stats(): LiveStatsSnapshot };
        }
      ).roLive.stats()
  );
}

function selectedKeys(page: Page): Promise<string[]> {
  return page.evaluate(() => window.roRowState.selectedKeys());
}

async function installLiveTransportProbe(
  page: Page,
  options: { holdFirstFrame?: boolean; stripResponseVersion?: boolean } = {}
): Promise<void> {
  await page.addInitScript(({ holdFirstFrame, stripResponseVersion }) => {
    const encoder = new TextEncoder();
    const nativeFetch = window.fetch.bind(window);
    let eventOrder = 0;
    let releaseFirstFrameGate!: () => void;
    const firstFrameGate = new Promise<void>((resolve) => {
      releaseFirstFrameGate = resolve;
    });
    let shouldHoldFirstFrame = holdFirstFrame;
    let shouldHoldNextHistoryReload = false;
    let historyReloadGate: Promise<void> | null = null;
    let releaseHistoryReloadGate: (() => void) | null = null;
    let currentStreamReader: ReadableStreamDefaultReader<Uint8Array> | null = null;
    const lifecycle: LifecycleProbe = {
      afterRequests: 0,
      afterSwaps: 0,
      beforeTransitions: 0,
      nativeStarts: 0,
      events: [],
    };
    const probe: LiveTransportProbe = {
      requests: [],
      responses: [],
      frames: [],
      lifecycle,
      fault: { kind: null, used: 0 },
      forcedEOFs: 0,
      firstFrameBlocked: false,
      historyReloadBlocked: false,
      historyReloadRequests: 0,
      async closeCurrentStream() {
        const reader = currentStreamReader;
        if (!reader) throw new Error('no active stream body to close');
        currentStreamReader = null;
        this.forcedEOFs += 1;
        await reader.cancel('test-induced downstream EOF');
      },
      holdNextHistoryReload() {
        if (shouldHoldNextHistoryReload || releaseHistoryReloadGate !== null) {
          throw new Error('a history reload is already held');
        }
        shouldHoldNextHistoryReload = true;
        historyReloadGate = new Promise<void>((resolve) => {
          releaseHistoryReloadGate = resolve;
        });
      },
      releaseHistoryReload() {
        const release = releaseHistoryReloadGate;
        shouldHoldNextHistoryReload = false;
        releaseHistoryReloadGate = null;
        historyReloadGate = null;
        release?.();
      },
      setFault(kind) {
        this.fault.kind = kind;
      },
      releaseFirstFrame() {
        releaseFirstFrameGate();
      },
    };
    (
      window as unknown as {
        __liveV2Probe: LiveTransportProbe;
      }
    ).__liveV2Probe = probe;

    const nativeHistoryGo = window.history.go.bind(window.history);
    Object.defineProperty(window.history, 'go', {
      configurable: true,
      value(delta?: number) {
        if (delta !== 0) {
          nativeHistoryGo(delta);
          return;
        }
        probe.historyReloadRequests += 1;
        if (!shouldHoldNextHistoryReload) {
          nativeHistoryGo(0);
          return;
        }
        shouldHoldNextHistoryReload = false;
        probe.historyReloadBlocked = true;
        const gate = historyReloadGate ?? Promise.resolve();
        void gate.then(() => {
          probe.historyReloadBlocked = false;
          nativeHistoryGo(0);
        });
      },
    });

    const isListLifecycle = (event: Event): boolean => {
      const detail = Object((event as CustomEvent).detail) as {
        elt?: { id?: unknown };
        target?: { id?: unknown };
      };
      const target = event.target;
      return (
        detail.elt?.id === 'resource-list-content' ||
        detail.target?.id === 'resource-list-content' ||
        (target instanceof Element &&
          (target.id === 'resource-list-content' ||
            target.closest('#resource-list-content') !== null))
      );
    };
    const lifecycleRequestURL = (event: Event): string | null => {
      const detail = Object((event as CustomEvent).detail) as {
        xhr?: { responseURL?: unknown };
        requestConfig?: { path?: unknown };
        pathInfo?: { requestPath?: unknown };
      };
      if (typeof detail.xhr?.responseURL === 'string' && detail.xhr.responseURL !== '') {
        return detail.xhr.responseURL;
      }
      const path = detail.requestConfig?.path ?? detail.pathInfo?.requestPath;
      return typeof path === 'string' ? path : null;
    };
    const recordLifecycle = (
      kind: string,
      event?: Event,
      requestURL: string | null = null
    ): void => {
      lifecycle.events.push({
        order: ++eventOrder,
        kind,
        location: window.location.href,
        requestURL: requestURL ?? (event ? lifecycleRequestURL(event) : null),
      });
    };
    document.addEventListener('htmx:beforeRequest', (event) => {
      if (!isListLifecycle(event)) return;
      recordLifecycle('htmx:beforeRequest', event);
      const detail = Object((event as CustomEvent).detail) as {
        xhr?: XMLHttpRequest;
      };
      detail.xhr?.addEventListener(
        'loadend',
        () => recordLifecycle('xhr:loadend', event, detail.xhr?.responseURL ?? null),
        { once: true }
      );
    });
    document.addEventListener('htmx:historyCacheHit', (event) => {
      recordLifecycle('htmx:historyCacheHit', event);
    });
    document.addEventListener('htmx:historyCacheMiss', (event) => {
      recordLifecycle('htmx:historyCacheMiss', event);
    });
    document.addEventListener('htmx:historyCacheMissLoad', (event) => {
      recordLifecycle('htmx:historyCacheMissLoad', event);
    });
    document.addEventListener('htmx:historyCacheMissLoadError', (event) => {
      recordLifecycle('htmx:historyCacheMissLoadError', event);
    });
    document.addEventListener('htmx:historyRestore', (event) => {
      recordLifecycle('htmx:historyRestore', event);
    });
    document.addEventListener('htmx:beforeSwap', (event) => {
      if (isListLifecycle(event)) recordLifecycle('htmx:beforeSwap', event);
    });
    document.addEventListener('htmx:afterRequest', (event) => {
      if (isListLifecycle(event)) {
        lifecycle.afterRequests += 1;
        recordLifecycle('htmx:afterRequest', event);
      }
    });
    document.addEventListener('htmx:afterSwap', (event) => {
      if (isListLifecycle(event)) {
        lifecycle.afterSwaps += 1;
        recordLifecycle('htmx:afterSwap', event);
      }
    });
    document.addEventListener('htmx:load', (event) => {
      if (isListLifecycle(event)) recordLifecycle('htmx:load', event);
    });
    document.addEventListener('htmx:beforeTransition', (event) => {
      if (isListLifecycle(event)) lifecycle.beforeTransitions += 1;
    });

    const nativeStart = document.startViewTransition;
    if (typeof nativeStart === 'function') {
      Object.defineProperty(document, 'startViewTransition', {
        configurable: true,
        value: function startViewTransition(
          this: Document,
          ...args: Parameters<Document['startViewTransition']>
        ) {
          const transition = Reflect.apply(nativeStart, this, args);
          lifecycle.nativeStarts += 1;
          return transition;
        },
      });
    }

    const findBoundary = (buffer: string): { index: number; length: number } | null => {
      const lf = buffer.indexOf('\n\n');
      const crlf = buffer.indexOf('\r\n\r\n');
      if (lf < 0 && crlf < 0) return null;
      if (lf >= 0 && (crlf < 0 || lf < crlf)) return { index: lf, length: 2 };
      return { index: crlf, length: 4 };
    };

    const ownString = (record: Record<string, unknown>, key: string): string | null =>
      typeof record[key] === 'string' ? (record[key] as string) : null;
    const ownNumber = (record: Record<string, unknown>, key: string): number | null =>
      typeof record[key] === 'number' ? (record[key] as number) : null;

    const transformFrame = (raw: string): string => {
      const fieldBlock = raw.replace(/(?:\r\n\r\n|\n\n)$/u, '');
      let event: string | null = null;
      const dataLines: string[] = [];
      for (const line of fieldBlock.split(/\r?\n/u)) {
        if (line.startsWith('event:')) {
          event = line.slice('event:'.length).replace(/^ /u, '');
        } else if (line.startsWith('data:')) {
          dataLines.push(line.slice('data:'.length).replace(/^ /u, ''));
        }
      }
      if (dataLines.length === 0) return raw; // heartbeat/comment

      const originalData = dataLines.join('\n');
      let parsed: Record<string, unknown> | null = null;
      try {
        const value = JSON.parse(originalData) as unknown;
        if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
          parsed = value as Record<string, unknown>;
        }
      } catch {
        // The production parser owns malformed-input behavior. The probe still
        // records the exact byte count and forwards the bytes untouched.
      }

      let deliveredData = originalData;
      let faultApplied: FaultKind | null = null;
      if (
        event === 'ro-live' &&
        parsed?.kind === 'delta' &&
        probe.fault.kind !== null
      ) {
        faultApplied = probe.fault.kind;
        probe.fault.kind = null;
        probe.fault.used += 1;
        if (faultApplied === 'corrupt') {
          deliveredData = '{"v":2,"kind":"delta"';
        } else {
          const damaged = structuredClone(parsed);
          if (faultApplied === 'gap' && typeof damaged.seq === 'number') {
            damaged.seq += 2;
          }
          if (faultApplied === 'schema' && typeof damaged.schema === 'string') {
            damaged.schema = `${damaged.schema}x`;
          }
          deliveredData = JSON.stringify(damaged);
        }
      }

      const delta =
        parsed?.delta !== null &&
        typeof parsed?.delta === 'object' &&
        !Array.isArray(parsed.delta)
          ? (parsed.delta as Record<string, unknown>)
          : null;
      const snapshot =
        parsed?.snapshot !== null &&
        typeof parsed?.snapshot === 'object' &&
        !Array.isArray(parsed.snapshot)
          ? (parsed.snapshot as Record<string, unknown>)
          : null;
      const upsert = Array.isArray(delta?.upsert) ? delta.upsert : [];
      const remove = Array.isArray(delta?.remove) ? delta.remove : [];
      const envelopeKind = parsed ? ownString(parsed, 'kind') : null;
      probe.frames.push({
        event,
        kind:
          envelopeKind ??
          'unknown',
        v: parsed ? ownNumber(parsed, 'v') : null,
        g: parsed ? ownString(parsed, 'g') : null,
        seq: parsed ? ownNumber(parsed, 'seq') : null,
        rev: parsed ? ownString(parsed, 'rev') : null,
        schema: parsed ? ownString(parsed, 'schema') : null,
        deltaBase: delta ? ownString(delta, 'base') : null,
        upsertKeys: upsert.flatMap((entry) => {
          if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) return [];
          const key = (entry as Record<string, unknown>).key;
          return typeof key === 'string' ? [key] : [];
        }),
        removeKeys: remove.flatMap((entry) => {
          if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) return [];
          const key = (entry as Record<string, unknown>).key;
          return typeof key === 'string' ? [key] : [];
        }),
        orderLength: Array.isArray(delta?.order) ? delta.order.length : null,
        payloadBytes: encoder.encode(originalData).byteLength,
        rawBytes: encoder.encode(raw).byteLength,
        snapshotHTMLBytes:
          typeof snapshot?.html === 'string' ? encoder.encode(snapshot.html).byteLength : null,
        faultApplied,
      });

      if (faultApplied === null) return raw;
      const eventLine = event === null ? '' : `event: ${event}\n`;
      return `${eventLine}data: ${deliveredData}\n\n`;
    };

    const wrapBody = (source: ReadableStream<Uint8Array>): ReadableStream<Uint8Array> => {
      const reader = source.getReader();
      currentStreamReader = reader;
      const decoder = new TextDecoder();
      let buffered = '';
      let ended = false;
      return new ReadableStream<Uint8Array>({
        async pull(controller) {
          for (;;) {
            const boundary = findBoundary(buffered);
            if (boundary !== null) {
              const end = boundary.index + boundary.length;
              const frame = buffered.slice(0, end);
              buffered = buffered.slice(end);
              if (shouldHoldFirstFrame) {
                shouldHoldFirstFrame = false;
                probe.firstFrameBlocked = true;
                await firstFrameGate;
                probe.firstFrameBlocked = false;
              }
              controller.enqueue(encoder.encode(transformFrame(frame)));
              return;
            }
            if (ended) {
              if (buffered.length > 0) {
                controller.enqueue(encoder.encode(buffered));
                buffered = '';
              } else {
                controller.close();
              }
              return;
            }
            try {
              const chunk = await reader.read();
              if (chunk.done) {
                if (currentStreamReader === reader) currentStreamReader = null;
                buffered += decoder.decode();
                ended = true;
              } else {
                buffered += decoder.decode(chunk.value, { stream: true });
              }
            } catch (error) {
              controller.error(error);
              return;
            }
          }
        },
        async cancel(reason) {
          if (currentStreamReader === reader) currentStreamReader = null;
          try {
            await reader.cancel(reason);
          } catch {
            // Abort-driven cancellation is the normal stream supersession path.
          }
        },
      });
    };

    window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      if (!url.pathname.endsWith('/_stream')) {
        return nativeFetch(input, init);
      }
      probe.requests.push({
        order: ++eventOrder,
        url: request.url,
        location: window.location.href,
        projectionRows: Array.isArray(
          (
            window as unknown as {
              roRowModel?: { rows?: unknown };
            }
          ).roRowModel?.rows
        )
          ? (
              window as unknown as {
                roRowModel: { rows: unknown[] };
              }
            ).roRowModel.rows.length
          : null,
        version: request.headers.get('RO-Live-Version'),
        generation: request.headers.get('RO-Live-Generation'),
      });

      const response = await nativeFetch(request);
      const responseHeaders = new Headers(response.headers);
      if (stripResponseVersion) {
        responseHeaders.delete('RO-Live-Version');
        responseHeaders.delete('RO-Live-Generation');
      }
      probe.responses.push({
        url: response.url,
        status: response.status,
        contentType: responseHeaders.get('Content-Type'),
        version: responseHeaders.get('RO-Live-Version'),
        generation: responseHeaders.get('RO-Live-Generation'),
      });
      if (!response.body) return response;

      const wrapped = new Response(wrapBody(response.body), {
        status: response.status,
        statusText: response.statusText,
        headers: responseHeaders,
      });
      // A constructed Response has an empty URL. Preserve the native response
      // metadata for clients that inspect it; defining these own properties is
      // supported in Chromium, and the fallback still preserves all protocol
      // fields used by Live.
      try {
        Object.defineProperties(wrapped, {
          url: { configurable: true, value: response.url },
          redirected: { configurable: true, value: response.redirected },
          type: { configurable: true, value: response.type },
        });
      } catch {
        // The Live client only requires status, headers, and body.
      }
      return wrapped;
    };
  }, {
    holdFirstFrame: options.holdFirstFrame === true,
    stripResponseVersion: options.stripResponseVersion === true,
  });
}

async function pickLive(page: Page): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator('.refresh-option[data-ro-interval="Live"]').click();
  await page.mouse.move(200, 400);
}

async function expectLiveFallbackBanner(page: Page): Promise<void> {
  const banner = page.locator('.ro-stale-banner');
  await expect(banner).toBeVisible();
  await expect(banner.locator('.bn-title')).toHaveText('Live unavailable, polling ·');
  await expect(banner.locator('[data-stale-countdown]')).toHaveText(/^[0-5]s$/u);
  await expect(banner.locator('[data-ro-action="retry"]')).toHaveText('Retry');
}

async function setHidden(page: Page, hidden: boolean): Promise<void> {
  await page.evaluate((value) => {
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => value,
    });
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (value ? 'hidden' : 'visible'),
    });
    document.dispatchEvent(new Event('visibilitychange'));
  }, hidden);
}

function uniqueGenerations(probe: LiveTransportSnapshot): string[] {
  return [
    ...new Set(
      probe.requests
        .map((request) => request.generation)
        .filter((generation): generation is string => generation !== null)
    ),
  ];
}

async function waitForGenerationCount(page: Page, count: number): Promise<void> {
  await expect
    .poll(async () => uniqueGenerations(await probeSnapshot(page)).length, { timeout: 10_000 })
    .toBe(count);
}

async function waitForFrame(
  page: Page,
  predicate: (frame: StreamFrameRecord) => boolean
): Promise<StreamFrameRecord> {
  await expect
    .poll(
      async () => (await probeSnapshot(page)).frames.findIndex(predicate),
      { timeout: 10_000 }
    )
    .toBeGreaterThanOrEqual(0);
  const frame = (await probeSnapshot(page)).frames.find(predicate);
  if (!frame) throw new Error('frame disappeared from the append-only probe');
  return frame;
}

async function waitForSnapshot(
  page: Page,
  generation: string,
  afterSwaps: number
): Promise<StreamFrameRecord> {
  const frame = await waitForFrame(
    page,
    (candidate) => candidate.event === 'ro-live' && candidate.kind === 'snapshot' && candidate.g === generation
  );
  const recordedSnapshots = (await probeSnapshot(page)).frames.filter(
    (candidate) => candidate.event === 'ro-live' && candidate.kind === 'snapshot'
  ).length;
  // The transport probe records before enqueueing the frame into the
  // production reader. v2Snapshots increments only after the synchronous HTMX
  // snapshot transaction completed, so this is the causal commit barrier.
  await expect
    .poll(
      async () => (await liveStats(page)).v2Snapshots,
      { timeout: 5_000 }
    )
    .toBeGreaterThanOrEqual(recordedSnapshots);
  await expect
    .poll(async () => (await probeSnapshot(page)).lifecycle.afterSwaps, { timeout: 5_000 })
    .toBeGreaterThan(afterSwaps);
  return frame;
}

async function openLiveV2(page: Page): Promise<{
  generation: string;
  snapshot: StreamFrameRecord;
}> {
  const before = await probeSnapshot(page);
  const requestPromise = page.waitForRequest(isStreamRequest, { timeout: 10_000 });
  await pickLive(page);
  const networkRequest = await requestPromise;
  const generation = networkRequest.headers()['ro-live-generation'];
  expect(generation).toBeTruthy();
  const snapshot = await waitForSnapshot(page, generation, before.lifecycle.afterSwaps);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
  return { generation, snapshot };
}

function bigPodKey(number: number): string {
  return `e2e/big/big-pod-${String(number).padStart(4, '0')}`;
}

function bigPodRow(page: Page, number: number) {
  return page.locator(`tr[data-key="${bigPodKey(number)}"]`);
}

async function setNextDeltaFault(page: Page, kind: FaultKind): Promise<void> {
  await page.evaluate((fault) => {
    (
      window as unknown as {
        __liveV2Probe: LiveTransportProbe;
      }
    ).__liveV2Probe.setFault(fault);
  }, kind);
}

async function closeCurrentStream(page: Page): Promise<void> {
  await page.evaluate(async () => {
    await (
      window as unknown as {
        __liveV2Probe: LiveTransportProbe;
      }
    ).__liveV2Probe.closeCurrentStream();
  });
}

interface HeldTableRequest {
  started: Promise<PlaywrightRequest>;
  release(): void;
}

async function holdNextStreamRequest(page: Page): Promise<HeldTableRequest> {
  let startedResolve!: (request: PlaywrightRequest) => void;
  let releaseResolve!: () => void;
  const started = new Promise<PlaywrightRequest>((resolve) => {
    startedResolve = resolve;
  });
  const released = new Promise<void>((resolve) => {
    releaseResolve = resolve;
  });
  await page.route(
    '**/_stream*',
    async (route) => {
      startedResolve(route.request());
      await released;
      try {
        await route.continue();
      } catch {
        // Some tests intentionally let the browser abort this held request.
      }
    },
    { times: 1 }
  );
  return { started, release: releaseResolve };
}

async function holdNextTableRequest(
  page: Page,
  outcome: 'continue' | 304 | 500 = 'continue'
): Promise<HeldTableRequest> {
  let startedResolve!: (request: PlaywrightRequest) => void;
  let releaseResolve!: () => void;
  const started = new Promise<PlaywrightRequest>((resolve) => {
    startedResolve = resolve;
  });
  const released = new Promise<void>((resolve) => {
    releaseResolve = resolve;
  });
  await page.route(
    '**/_table*',
    async (route) => {
      startedResolve(route.request());
      await released;
      if (outcome === 'continue') {
        await route.continue();
      } else if (outcome === 304) {
        await route.fulfill({ status: 304 });
      } else {
        await route.fulfill({
          status: 500,
          contentType: 'text/html; charset=utf-8',
          body: '<p>injected list failure</p>',
        });
      }
    },
    { times: 1 }
  );
  return { started, release: releaseResolve };
}

async function heldContainerRefresh(
  page: Page,
  outcome: 'continue' | 304 | 500,
  expectedStatus: number
): Promise<PlaywrightResponse> {
  const before = await probeSnapshot(page);
  const generationsBefore = uniqueGenerations(before).length;
  const held = await holdNextTableRequest(page, outcome);
  const responsePromise = page.waitForResponse(isContainerTableResponse, { timeout: 10_000 });
  await page.evaluate(() =>
    (
      window as unknown as {
        requestListRefresh(): void;
      }
    ).requestListRefresh()
  );
  await held.started;
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);
  await expect.poll(async () => (await liveStats(page)).inFlightRequests).toBe(1);
  expect(uniqueGenerations(await probeSnapshot(page))).toHaveLength(generationsBefore);
  held.release();
  const response = await responsePromise;
  expect(response.status()).toBe(expectedStatus);
  await expect
    .poll(async () => (await probeSnapshot(page)).lifecycle.afterRequests, { timeout: 5_000 })
    .toBeGreaterThan(before.lifecycle.afterRequests);
  await waitForGenerationCount(page, generationsBefore + 1);
  const next = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!next) throw new Error('settled table request did not mint a generation');
  await waitForSnapshot(page, next, before.lifecycle.afterSwaps);
  await expect.poll(async () => (await liveStats(page)).inFlightRequests).toBe(0);
  expect(uniqueGenerations(await probeSnapshot(page))).toHaveLength(generationsBefore + 1);
  return response;
}

test.beforeEach(async () => {
  await control('/__control/reset');
});

test('negotiates v2 with one unreserved generation and commits an echoed initial snapshot', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page);
  await page.goto(PODS);

  const { generation, snapshot } = await openLiveV2(page);
  expect(generation).toMatch(/^(?:[A-Fa-f0-9]{32}|[A-Fa-f0-9-]{36})$/u);

  const probe = await probeSnapshot(page);
  const request = probe.requests[0];
  const response = probe.responses[0];
  expect(request).toMatchObject({
    version: '2',
    generation,
  });
  expect(new URL(request.url).searchParams.has('g')).toBe(false);
  expect(response).toMatchObject({ status: 200, version: '2', generation });
  expect(response.contentType).toMatch(/^text\/event-stream(?:;|$)/u);
  expect(snapshot).toMatchObject({
    event: 'ro-live',
    kind: 'snapshot',
    v: 2,
    g: generation,
    seq: 1,
  });
  expect(snapshot.rev).toMatch(/^ro-live-v2-[A-Za-z0-9_-]{43}$/u);
  expect(snapshot.schema).toMatch(/^[A-Za-z0-9_-]{43}$/u);
  expect(snapshot.snapshotHTMLBytes).toBeGreaterThan(0);
  expect(snapshot.payloadBytes).toBeGreaterThan(snapshot.snapshotHTMLBytes ?? 0);
});

test('an ro-live frame completes v2 negotiation when a proxy strips response headers', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page, { stripResponseVersion: true });
  await page.goto(PODS);

  const { generation, snapshot } = await openLiveV2(page);
  expect(snapshot).toMatchObject({ event: 'ro-live', g: generation, kind: 'snapshot', v: 2 });
  expect((await probeSnapshot(page)).responses[0]).toMatchObject({
    status: 200,
    version: null,
    generation: null,
  });
  expect(await liveStats(page)).toMatchObject({ state: 'open', seq: 1 });
});

test('a stream fetch held before response headers enters visible fallback without opening a watch', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  await page.clock.install();
  const held = await holdNextStreamRequest(page);
  const failed = page.waitForEvent('requestfailed', {
    predicate: isStreamRequest,
    timeout: 10_000,
  });

  await pickLive(page);
  await held.started;
  expect(await liveStats(page)).toMatchObject({
    state: 'connecting',
    connections: 1,
    v2Snapshots: 0,
    fallbacks: 0,
  });
  expect((await probeSnapshot(page)).responses).toHaveLength(0);
  expect(await openWatchCount()).toBe(0);

  await page.clock.fastForward(30_001);
  await expect
    .poll(async () => (await liveStats(page)).state, { timeout: 5_000 })
    .toBe('fallback');
  held.release();
  await failed;
  expect(await liveStats(page)).toMatchObject({
    connections: 1,
    v2Snapshots: 0,
    fallbacks: 1,
  });
  const probe = await probeSnapshot(page);
  expect(probe.responses).toHaveLength(0);
  expect(probe.frames).toHaveLength(0);
  await expectLiveFallbackBanner(page);
  expect(await openWatchCount()).toBe(0);
});

test('a negotiated 200 stream without a first frame enters visible fallback and releases its watch', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page, { holdFirstFrame: true });
  await page.goto(PODS);
  await page.clock.install();

  const requestPromise = page.waitForRequest(isStreamRequest, { timeout: 10_000 });
  await pickLive(page);
  await requestPromise;
  await expect
    .poll(async () => (await probeSnapshot(page)).responses.length, { timeout: 5_000 })
    .toBe(1);
  await expect
    .poll(async () => (await probeSnapshot(page)).firstFrameBlocked, { timeout: 5_000 })
    .toBe(true);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
  expect(await liveStats(page)).toMatchObject({
    state: 'connecting',
    connections: 1,
    v2Snapshots: 0,
    fallbacks: 0,
  });

  // Advance the browser-owned first-frame deadline, not wall time. The server
  // and network remain real; the response has already negotiated 200 + SSE and
  // the upstream watch is demonstrably open.
  await page.clock.fastForward(30_001);
  await expect
    .poll(async () => (await liveStats(page)).state, { timeout: 5_000 })
    .toBe('fallback');
  expect(await liveStats(page)).toMatchObject({
    connections: 1,
    v2Snapshots: 0,
    fallbacks: 1,
  });
  await expectLiveFallbackBanner(page);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);
});

test('a later canceler of htmx:beforeRequest cannot strand Live suspended', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop table request lifecycle');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  await openLiveV2(page);
  const before = await probeSnapshot(page);
  const statsBefore = await liveStats(page);
  const tableRequests: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname.endsWith('/_table')) tableRequests.push(request.url());
  });

  await page.evaluate(() => {
    const state = { canceled: 0 };
    (
      window as unknown as {
        __liveV2CanceledRequest: { canceled: number };
      }
    ).__liveV2CanceledRequest = state;
    const cancelOnce = (event: Event) => {
      const detail = Object((event as CustomEvent).detail) as {
        target?: { id?: unknown };
      };
      if (detail.target?.id !== 'resource-list-content') return;
      state.canceled += 1;
      event.preventDefault();
      document.removeEventListener('htmx:beforeRequest', cancelOnce);
    };
    // Installed after the production listener: Live speculatively owns and
    // suspends first, then this independent extension cancels the request.
    document.addEventListener('htmx:beforeRequest', cancelOnce);
  });
  await page.evaluate(() =>
    (
      window as unknown as {
        requestListRefresh(): void;
      }
    ).requestListRefresh()
  );
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __liveV2CanceledRequest: { canceled: number };
            }
          ).__liveV2CanceledRequest.canceled
      )
    )
    .toBe(1);

  await waitForGenerationCount(page, uniqueGenerations(before).length + 1);
  const generation = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!generation) throw new Error('canceled request did not resume Live');
  await waitForSnapshot(page, generation, before.lifecycle.afterSwaps);
  expect(tableRequests).toEqual([]);
  expect(await liveStats(page)).toMatchObject({
    state: 'open',
    inFlightRequests: 0,
    connections: statsBefore.connections + 1,
  });
  expect(uniqueGenerations(await probeSnapshot(page))).toHaveLength(
    uniqueGenerations(before).length + 1
  );
});

test('an Off-mode table request already in flight makes an explicit Live pick wait for loadend and open once', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop table request lifecycle');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  expect(await liveStats(page)).toMatchObject({ state: 'off', connections: 0 });

  const before = await probeSnapshot(page);
  const held = await holdNextTableRequest(page, 'continue');
  const responsePromise = page.waitForResponse(isContainerTableResponse, { timeout: 10_000 });
  await page.evaluate(() =>
    (
      window as unknown as {
        requestListRefresh(): void;
      }
    ).requestListRefresh()
  );
  await held.started;
  await expect.poll(async () => (await liveStats(page)).inFlightRequests).toBe(1);
  expect((await probeSnapshot(page)).requests).toHaveLength(0);

  await pickLive(page);
  expect(await liveStats(page)).toMatchObject({
    state: 'suspended',
    connections: 0,
    inFlightRequests: 1,
  });
  expect((await probeSnapshot(page)).requests).toHaveLength(0);

  held.release();
  expect((await responsePromise).status()).toBe(200);
  await expect
    .poll(async () => (await probeSnapshot(page)).lifecycle.afterRequests, { timeout: 5_000 })
    .toBeGreaterThan(before.lifecycle.afterRequests);
  await waitForGenerationCount(page, 1);
  const generation = uniqueGenerations(await probeSnapshot(page))[0];
  await waitForSnapshot(page, generation, before.lifecycle.afterSwaps);
  expect(uniqueGenerations(await probeSnapshot(page))).toEqual([generation]);
  expect(await liveStats(page)).toMatchObject({
    state: 'open',
    connections: 1,
    inFlightRequests: 0,
  });
});

test('a 600-row one-cell delta stays under both wire budgets, preserves identity, and updates an offscreen canonical row', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop windowing surface');
  await installLiveTransportProbe(page);
  await page.goto(BIG_PODS);
  const { generation, snapshot } = await openLiveV2(page);

  await page.evaluate((key) => {
    (
      window as unknown as {
        __liveV2UntouchedRow: Element | null;
      }
    ).__liveV2UntouchedRow = document.querySelector(`tr[data-key="${key}"]`);
  }, BIG_UNTOUCHED_KEY);
  const visibleCellsBefore = (await bigPodRow(page, 2).locator('td').allTextContents()).map(
    (cell) => cell.trim()
  );
  const visibleCellsUpdate = visibleCellsBefore.slice(0, 5);
  visibleCellsUpdate[2] = 'Error';
  const before = await probeSnapshot(page);
  const statsBefore = await liveStats(page);

  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'big-pod-0002',
          namespace: 'big',
          creationTimestamp: '2026-06-08T12:00:00Z',
        },
      },
      cells: visibleCellsUpdate,
    },
  ]);
  const delta = await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.upsertKeys.includes(BIG_VISIBLE_KEY)
  );
  await expect(bigPodRow(page, 2).locator('td').nth(2)).toContainText('Error');
  const visibleCellsAfter = (await bigPodRow(page, 2).locator('td').allTextContents()).map(
    (cell) => cell.trim()
  );
  expect(
    visibleCellsAfter.flatMap((cell, index) =>
      cell === visibleCellsBefore[index] ? [] : [index]
    )
  ).toEqual([2]);
  expect(delta.event).toBe('ro-live');
  expect(delta.payloadBytes).toBeLessThanOrEqual(4 * 1024);
  expect(delta.payloadBytes * 100).toBeLessThanOrEqual(snapshot.payloadBytes);
  expect(delta.deltaBase).toBe(snapshot.rev);
  expect(delta.seq).toBe(2);
  expect(delta.upsertKeys).toEqual([BIG_VISIBLE_KEY]);
  expect(delta.orderLength).toBeNull();
  const statsAfterVisible = await liveStats(page);
  expect(statsAfterVisible.deltas).toBe(statsBefore.deltas + 1);
  expect(statsAfterVisible.deltaBytes).toBe(statsBefore.deltaBytes + delta.payloadBytes);
  expect(statsAfterVisible.seq).toBe(2);
  expect(
    await page.evaluate((key) => {
      const remembered = (
        window as unknown as {
          __liveV2UntouchedRow: Element | null;
        }
      ).__liveV2UntouchedRow;
      return remembered === document.querySelector(`tr[data-key="${key}"]`);
    }, BIG_UNTOUCHED_KEY)
  ).toBe(true);
  const afterVisible = await probeSnapshot(page);
  expect(afterVisible.lifecycle.afterSwaps).toBe(before.lifecycle.afterSwaps);
  expect(afterVisible.lifecycle.beforeTransitions).toBe(before.lifecycle.beforeTransitions);
  expect(afterVisible.lifecycle.nativeStarts).toBe(before.lifecycle.nativeStarts);

  // The target is outside the rendered tbody window. The delta must update the
  // canonical projection without inserting 550 into the current DOM slice.
  await expect(page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"]`)).toHaveCount(0);
  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'big-pod-0550',
          namespace: 'big',
          creationTimestamp: '2026-06-08T12:00:00Z',
        },
      },
      cells: ['big-pod-0550', '0/1', 'OffscreenError', '9', '10m'],
    },
  ]);
  await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.upsertKeys.includes(BIG_OFFSCREEN_KEY)
  );
  await expect
    .poll(
      () =>
        page.evaluate((key) => {
          const model = (
            window as unknown as {
              roRowModel: { rows: { key: string; cells: string[] }[] };
            }
          ).roRowModel;
          const row = model.rows.find((candidate) => candidate.key === key);
          return row?.cells.includes('OffscreenError') ?? false;
        }, BIG_OFFSCREEN_KEY),
      { timeout: 5_000 }
    )
    .toBe(true);
  expect(await page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"]`).count()).toBe(0);
  expect(
    await page.evaluate(
      (key) =>
        (
          window as unknown as {
            roVirtual: { scrollToKey(value: string): boolean };
          }
        ).roVirtual.scrollToKey(key),
      BIG_OFFSCREEN_KEY
    )
  ).toBe(true);
  await expect(page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"]`)).toBeVisible();
  await expect(page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"] td`).nth(2)).toContainText(
    'OffscreenError'
  );
});

test('a mobile delta updates the closed card island without an HTMX swap', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile card projection');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  const { generation } = await openLiveV2(page);
  const before = await probeSnapshot(page);
  const card = page.locator(`.ro-pcard[data-key="${NGINX_KEY}"]`);
  await expect(card).toBeVisible();

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
          creationTimestamp: '2024-03-01T10:00:00Z',
        },
      },
      cells: ['nginx', '0/1', 'MobileError', '3', '10m'],
    },
  ]);
  const delta = await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.upsertKeys.includes(NGINX_KEY)
  );
  await expect(card.locator('.pc-status')).toContainText('MobileError');
  expect(delta.payloadBytes).toBeLessThanOrEqual(4 * 1024);
  const after = await probeSnapshot(page);
  expect(after.lifecycle.afterSwaps).toBe(before.lifecycle.afterSwaps);
  expect(after.lifecycle.nativeStarts).toBe(before.lifecycle.nativeStarts);
});

test('a sorted Live insert carries order and avoids an HTMX swap', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop sorted table surface');
  await installLiveTransportProbe(page);
  await page.goto(`${PODS}?sort=Name`);
  const { generation } = await openLiveV2(page);
  const before = await probeSnapshot(page);

  const addedKey = 'e2e/default/aaa-live';
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'aaa-live',
          namespace: 'default',
          creationTimestamp: '2026-06-10T12:00:00Z',
        },
        status: { phase: 'Running' },
      },
      cells: ['aaa-live', '1/1', 'Running', '0', '1m'],
    },
  ]);

  const delta = await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.upsertKeys.includes(addedKey)
  );
  await expect(page.locator(`tr[data-key="${addedKey}"]`)).toBeVisible();
  expect(delta.orderLength).toBe(3);
  expect(
    await page.locator('#resource-list-content tbody tr[data-key]').evaluateAll((rows) =>
      rows.map((row) => (row as HTMLElement).dataset.key)
    )
  ).toEqual([addedKey, 'e2e/default/my-app', NGINX_KEY]);
  expect((await probeSnapshot(page)).lifecycle.afterSwaps).toBe(before.lifecycle.afterSwaps);
});

test('a Live reorder preserves focus on the existing row that actually moves', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop sorted table surface');
  await installLiveTransportProbe(page);
  await page.goto(`${PODS}?sort=Status`);
  const { generation } = await openLiveV2(page);
  const before = await probeSnapshot(page);
  const rows = page.locator('#resource-list-content tbody tr[data-key]');
  await expect(rows).toHaveCount(2);
  expect(
    await rows.evaluateAll((items) =>
      items.map((item) => (item as HTMLElement).dataset.key)
    )
  ).toEqual(['e2e/default/my-app', NGINX_KEY]);

  const focused = page.locator(`tr[data-key="${NGINX_KEY}"] a`).first();
  await focused.focus();
  await expect(focused).toBeFocused();
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
          creationTimestamp: '2024-03-01T10:00:00Z',
        },
        status: { phase: 'Pending' },
      },
      cells: ['nginx', '0/1', 'CrashLoopBackOff', '1', '10m'],
    },
  ]);

  const delta = await waitForFrame(
    page,
    (frame) =>
      frame.kind === 'delta' &&
      frame.g === generation &&
      frame.upsertKeys.includes(NGINX_KEY)
  );
  expect(delta.orderLength).toBe(2);
  await expect(rows).toHaveCount(2);
  expect(
    await rows.evaluateAll((items) =>
      items.map((item) => (item as HTMLElement).dataset.key)
    )
  ).toEqual([NGINX_KEY, 'e2e/default/my-app']);
  await expect(focused).toBeFocused();
  expect((await probeSnapshot(page)).lifecycle.afterSwaps).toBe(before.lifecycle.afterSwaps);
});

test('a Live delete clears selection with the deleted row', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop row-selection surface');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  const { generation } = await openLiveV2(page);
  await page.locator(`tr[data-key="${NGINX_KEY}"] td`).nth(1).click();
  expect(await selectedKeys(page)).toEqual([NGINX_KEY]);
  await expect(page.locator('#ro-bulkbar')).toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');

  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'DELETED',
      object: { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'nginx', namespace: 'default' } },
    },
  ]);
  const delta = await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.removeKeys.includes(NGINX_KEY)
  );
  expect(delta.orderLength).toBe(1);
  await expect(page.locator(`tr[data-key="${NGINX_KEY}"]`)).toHaveCount(0);
  expect(await selectedKeys(page)).toEqual([]);
  await expect(page.locator('#ro-bulkbar')).not.toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('0 selected');
});

test('a row projected out by a Live filter keeps its selection identity', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop row-selection surface');
  await installLiveTransportProbe(page);
  await page.goto(`${PODS}?f=status%3ARunning`);
  const { generation } = await openLiveV2(page);
  const projectedKey = 'e2e/default/my-app';
  await page.locator(`tr[data-key="${projectedKey}"] td`).nth(1).click();
  expect(await selectedKeys(page)).toEqual([projectedKey]);
  await expect(page.locator('#ro-bulkbar')).toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');

  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'my-app',
          namespace: 'default',
          creationTimestamp: '2024-03-02T11:30:00Z',
        },
        status: { phase: 'Pending' },
      },
      cells: ['my-app', '0/1', 'Pending', '0', '5m'],
    },
  ]);
  await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.removeKeys.includes(projectedKey)
  );
  await expect(page.locator(`tr[data-key="${projectedKey}"]`)).toHaveCount(0);
  expect(await selectedKeys(page)).toEqual([projectedKey]);
  await expect(page.locator('#ro-bulkbar')).toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('1 selected');
});

test('an Event with an unknown involved kind applies as a Live delta without resync', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Events table surface');
  await installLiveTransportProbe(page);
  await page.goto(EVENTS);
  const { generation } = await openLiveV2(page);
  const before = await liveStats(page);
  const eventKey = 'e2e/default/mystery.0005';

  await scriptEvents([
    {
      path: EVENTS_LIST_PATH,
      type: 'ADDED',
      object: {
        apiVersion: 'v1',
        kind: 'Event',
        metadata: {
          name: 'mystery.0005',
          namespace: 'default',
          creationTimestamp: '2026-06-10T12:00:00Z',
        },
        type: 'Normal',
        reason: 'Observed',
        message: 'Observed an object with an unknown kind',
        count: 1,
        firstTimestamp: '2026-06-10T12:00:00Z',
        lastTimestamp: '2026-06-10T12:00:00Z',
        involvedObject: { kind: 'Widget', name: 'sample', namespace: 'default' },
      },
      cells: ['1s', 'Normal', 'Observed', 'widget/sample', 'Observed an unknown kind'],
    },
  ]);
  await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.upsertKeys.includes(eventKey)
  );
  const row = page.locator(`tr[data-key="${eventKey}"]`);
  await expect(row).toBeVisible();
  await expect(row.locator('.kind-tile')).toHaveAttribute('style', /--kh:/u);
  expect(await liveStats(page)).toMatchObject({
    resyncs: before.resyncs,
    fallbacks: before.fallbacks,
    invalidFrames: before.invalidFrames,
  });
  expect(uniqueGenerations(await probeSnapshot(page))).toEqual([generation]);
});

test('mid-stream EOF reopens once with a fresh generation and one snapshot', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  const first = await openLiveV2(page);
  await page.clock.install();
  const statsBefore = await liveStats(page);
  const beforeEOF = await probeSnapshot(page);

  await closeCurrentStream(page);
  await expect.poll(async () => (await liveStats(page)).state, { timeout: 5_000 }).toBe('fallback');
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);
  expect((await probeSnapshot(page)).forcedEOFs).toBe(1);

  // Drive the browser-owned retry horizon in two steps and settle the first
  // fallback poll between them, so request ownership cannot make the retry
  // outcome ambiguous.
  await page.clock.fastForward(59_999);
  await expect
    .poll(async () => (await probeSnapshot(page)).lifecycle.afterRequests, { timeout: 5_000 })
    .toBeGreaterThan(beforeEOF.lifecycle.afterRequests);
  await expect.poll(async () => (await liveStats(page)).inFlightRequests).toBe(0);
  await page.clock.fastForward(1);

  await waitForGenerationCount(page, 2);
  const secondGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!secondGeneration) throw new Error('EOF did not reopen Live');
  expect(secondGeneration).not.toBe(first.generation);
  await waitForFrame(
    page,
    (frame) =>
      frame.event === 'ro-live' &&
      frame.kind === 'snapshot' &&
      frame.g === secondGeneration
  );
  // v2Snapshots increments only after htmx.swap synchronously emitted the
  // transaction-marked afterSwap and Live accepted that exact transaction.
  await expect
    .poll(async () => (await liveStats(page)).v2Snapshots, { timeout: 5_000 })
    .toBe(statsBefore.v2Snapshots + 1);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(1);
  const after = await probeSnapshot(page);
  const reopened = after.requests.slice(beforeEOF.requests.length);
  expect(reopened).toHaveLength(1);
  expect(reopened[0].generation).toBe(secondGeneration);
  expect(
    after.frames
      .slice(beforeEOF.frames.length)
      .filter((frame) => frame.kind === 'snapshot' && frame.g === secondGeneration)
  ).toHaveLength(1);
  expect(uniqueGenerations(after)).toEqual([first.generation, secondGeneration]);
  expect(await liveStats(page)).toMatchObject({
    state: 'open',
    connections: statsBefore.connections + 1,
    fallbacks: statsBefore.fallbacks + 1,
    v2Snapshots: statsBefore.v2Snapshots + 1,
  });
});

test('all-namespaces Pods opens and applies Live v2', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page);
  await page.goto(ALL_PODS);
  const { generation } = await openLiveV2(page);
  const opened = await probeSnapshot(page);
  const request = opened.requests.find((candidate) => candidate.generation === generation);
  if (!request) throw new Error('all-namespaces Live request was not recorded');
  expect(new URL(request.url).pathname).toBe(`${ALL_PODS}/_stream`);
  await expect(page.locator(`tr[data-key="${NGINX_KEY}"] td.cell-ns`)).toHaveText('default');
  const before = await liveStats(page);

  await scriptEvents([
    {
      path: ALL_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'nginx',
          namespace: 'default',
          creationTimestamp: '2024-03-01T10:00:00Z',
        },
        status: { phase: 'Running' },
      },
      cells: ['nginx', '1/1', 'AllNamespacesLive', '0', '10m'],
    },
  ]);
  await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === generation && frame.upsertKeys.includes(NGINX_KEY)
  );
  await expect(page.locator(`tr[data-key="${NGINX_KEY}"]`)).toContainText('AllNamespacesLive');
  await expect(page.locator(`tr[data-key="${NGINX_KEY}"] td.cell-ns`)).toHaveText('default');
  expect((await liveStats(page)).deltas).toBe(before.deltas + 1);
});

test('held 200, 304, and 500 table requests suspend the stream and each settlement mints exactly one generation', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop table request lifecycle');
  test.setTimeout(60_000);
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  await openLiveV2(page);

  expect((await heldContainerRefresh(page, 'continue', 200)).status()).toBe(200);
  expect((await heldContainerRefresh(page, 304, 304)).status()).toBe(304);
  expect((await heldContainerRefresh(page, 500, 500)).status()).toBe(500);

  const probe = await probeSnapshot(page);
  expect(uniqueGenerations(probe)).toHaveLength(4);
  for (const generation of uniqueGenerations(probe)) {
    expect(
      probe.frames.filter((frame) => frame.kind === 'snapshot' && frame.g === generation)
    ).toHaveLength(1);
  }
});

test('an aborted container refresh plus a concurrent user sort reopens only after the winning request settles', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop concurrent request surface');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  await openLiveV2(page);
  const before = await probeSnapshot(page);
  const generationsBefore = uniqueGenerations(before).length;

  let firstRelease!: () => void;
  let secondRelease!: () => void;
  let firstStartedResolve!: (request: PlaywrightRequest) => void;
  let secondStartedResolve!: (request: PlaywrightRequest) => void;
  const firstStarted = new Promise<PlaywrightRequest>((resolve) => {
    firstStartedResolve = resolve;
  });
  const secondStarted = new Promise<PlaywrightRequest>((resolve) => {
    secondStartedResolve = resolve;
  });
  const firstGate = new Promise<void>((resolve) => {
    firstRelease = resolve;
  });
  const secondGate = new Promise<void>((resolve) => {
    secondRelease = resolve;
  });
  let requestIndex = 0;
  await page.route('**/_table*', async (route: Route) => {
    requestIndex += 1;
    if (requestIndex === 1) {
      firstStartedResolve(route.request());
      await firstGate;
    } else {
      secondStartedResolve(route.request());
      await secondGate;
    }
    try {
      await route.continue();
    } catch {
      // The user request intentionally aborts the held container refresh.
    }
  });

  const aborted = page.waitForEvent('requestfailed', {
    predicate: (request) =>
      new URL(request.url()).pathname.endsWith('/_table') &&
      request.headers()['ro-no-push'] === 'true',
    timeout: 10_000,
  });
  await page.evaluate(() =>
    (
      window as unknown as {
        requestListRefresh(): void;
      }
    ).requestListRefresh()
  );
  await firstStarted;
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);

  const sortResponse = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname.endsWith('/_table') &&
      response.request().headers()['ro-no-push'] !== 'true',
    { timeout: 10_000 }
  );
  await page.locator('thead th a', { hasText: 'Name' }).first().click();
  await secondStarted;
  firstRelease();
  await aborted;
  expect(uniqueGenerations(await probeSnapshot(page))).toHaveLength(generationsBefore);

  secondRelease();
  expect((await sortResponse).status()).toBe(200);
  await expect
    .poll(
      async () =>
        (await probeSnapshot(page)).lifecycle.events.some(
          (event) =>
            event.order > (before.lifecycle.events.at(-1)?.order ?? 0) &&
            event.kind === 'xhr:loadend' &&
            event.requestURL?.includes('sort=Name') === true
        ),
      { timeout: 10_000 }
    )
    .toBe(true);
  const after = await probeSnapshot(page);
  const lifecycleOrderBefore = before.lifecycle.events.at(-1)?.order ?? 0;
  const reopened = after.requests.slice(before.requests.length);
  await testInfo.attach('concurrent-request-lifecycle.json', {
    body: JSON.stringify(
      {
        streams: reopened,
        events: after.lifecycle.events.filter((event) => event.order > lifecycleOrderBefore),
      },
      null,
      2
    ),
    contentType: 'application/json',
  });
  expect(reopened).toHaveLength(1);
  expect(new URL(reopened[0].url).searchParams.get('sort')).toBe('Name');
  const generation = reopened[0].generation;
  if (!generation) throw new Error('reopened Live request had no generation header');
  await waitForSnapshot(page, generation, before.lifecycle.afterSwaps);
  expect(uniqueGenerations(after)).toHaveLength(generationsBefore + 1);
  await page.unroute('**/_table*');
});

for (const fault of ['corrupt', 'gap', 'schema'] as const) {
  test(`a ${fault} delta is rejected atomically and repaired by one fresh snapshot`, async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
    await installLiveTransportProbe(page);
    await page.goto(PODS);
    const { generation } = await openLiveV2(page);
    const before = await probeSnapshot(page);
    const statsBefore = await liveStats(page);
    const generationsBefore = uniqueGenerations(before).length;
    const statusCell = page.locator(`tr[data-key="${NGINX_KEY}"] td:has(span.cell-status)`);
    const originalStatus = await statusCell.textContent();
    const heldResync = await holdNextStreamRequest(page);
    await setNextDeltaFault(page, fault);

    const status = `Fault-${fault}`;
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
            creationTimestamp: '2024-03-01T10:00:00Z',
          },
        },
        cells: ['nginx', '0/1', status, '3', '10m'],
      },
    ]);
    await waitForFrame(
      page,
      (frame) => frame.kind === 'delta' && frame.g === generation && frame.faultApplied === fault
    );
    await heldResync.started;
    // The rejected delta never touched the current DOM. Hold its replacement
    // snapshot at the network boundary so this observation cannot race repair.
    expect(await statusCell.textContent()).toBe(originalStatus);
    heldResync.release();
    await waitForGenerationCount(page, generationsBefore + 1);
    const repairedGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
    if (!repairedGeneration) throw new Error('invalid frame did not start resync');
    const repaired = await waitForSnapshot(
      page,
      repairedGeneration,
      before.lifecycle.afterSwaps
    );
    expect(repaired.seq).toBe(1);
    expect(repaired.g).not.toBe(generation);
    await expect(
      page.locator(`tr[data-key="${NGINX_KEY}"] td:has(span.cell-status)`)
    ).toContainText(status);
    const after = await probeSnapshot(page);
    expect(after.fault.used).toBe(1);
    expect(uniqueGenerations(after)).toHaveLength(generationsBefore + 1);
    const statsAfter = await liveStats(page);
    expect(statsAfter.invalidFrames).toBe(statsBefore.invalidFrames + 1);
    expect(statsAfter.resyncs).toBe(statsBefore.resyncs + 1);
    expect(statsAfter.connections).toBe(statsBefore.connections + 1);
    expect(statsAfter.fallbacks).toBe(statsBefore.fallbacks);
    expect(statsAfter.resyncsInWindow).toBe(1);
    expect(statsAfter).toMatchObject({ state: 'open', seq: 1 });
  });
}

test('hidden close reopens v2 with a fresh seq-1 snapshot, and only snapshots dispatch afterSwap', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop Live controls');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  const first = await openLiveV2(page);
  const afterFirst = await probeSnapshot(page);

  await setHidden(page, true);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);
  await setHidden(page, false);
  await waitForGenerationCount(page, 2);
  const secondGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!secondGeneration) throw new Error('visibility return did not reopen Live');
  const second = await waitForSnapshot(
    page,
    secondGeneration,
    afterFirst.lifecycle.afterSwaps
  );
  expect(secondGeneration).not.toBe(first.generation);
  expect(second.seq).toBe(1);
  expect((await probeSnapshot(page)).lifecycle.afterSwaps).toBe(
    afterFirst.lifecycle.afterSwaps + 1
  );

  const beforeDelta = await probeSnapshot(page);
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
          creationTimestamp: '2024-03-01T10:00:00Z',
        },
      },
      cells: ['nginx', '1/1', 'Running', '8', '10m'],
    },
  ]);
  await waitForFrame(
    page,
    (frame) => frame.kind === 'delta' && frame.g === secondGeneration
  );
  await expect(page.locator(`tr[data-key="${NGINX_KEY}"] td`).nth(3)).toContainText('8');
  const afterDelta = await probeSnapshot(page);
  expect(afterDelta.lifecycle.afterSwaps).toBe(beforeDelta.lifecycle.afterSwaps);
  expect(afterDelta.lifecycle.beforeTransitions).toBe(beforeDelta.lifecycle.beforeTransitions);
  expect(afterDelta.lifecycle.nativeStarts).toBe(beforeDelta.lifecycle.nativeStarts);
});

test('a user sort stays transition-free while Live suspends and resumes', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop sort surface');
  await installLiveTransportProbe(page);
  await page.goto(PODS);
  await openLiveV2(page);
  const before = await probeSnapshot(page);
  const responsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname.endsWith('/_table') &&
      response.request().headers()['ro-no-push'] !== 'true',
    { timeout: 10_000 }
  );

  await page.locator('thead th a', { hasText: 'Name' }).first().click();
  expect((await responsePromise).status()).toBe(200);
  await expect
    .poll(async () => (await probeSnapshot(page)).lifecycle.afterSwaps, { timeout: 5_000 })
    .toBeGreaterThan(before.lifecycle.afterSwaps);
  const afterSwap = await probeSnapshot(page);
  expect(afterSwap.lifecycle.beforeTransitions).toBe(before.lifecycle.beforeTransitions);
  expect(afterSwap.lifecycle.nativeStarts).toBe(before.lifecycle.nativeStarts);
  await expect(page).toHaveURL(/\?sort=Name$/u);
  await waitForGenerationCount(page, uniqueGenerations(before).length + 1);
  const generation = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!generation) throw new Error('sort settlement did not reopen Live');
  await waitForSnapshot(page, generation, before.lifecycle.afterSwaps);
});

test('a transition-free cache-hit Back opens one v2 stream only after the restored full projection lands', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop history and windowing surface');
  test.setTimeout(60_000);
  await installLiveTransportProbe(page);
  await page.goto(BIG_PODS);
  await openLiveV2(page);
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              roRowModel: { rows: { key: string; cells: string[] }[] };
            }
          ).roRowModel.rows.length
      )
    )
    .toBe(600);

  const beforeEvents = await probeSnapshot(page);
  const eventsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_EVENTS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Events' }).click();
  expect((await eventsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
  await waitForGenerationCount(page, uniqueGenerations(beforeEvents).length + 1);
  const eventsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!eventsGeneration) throw new Error('events navigation did not reopen Live');
  const eventsSnapshot = await waitForSnapshot(
    page,
    eventsGeneration,
    beforeEvents.lifecycle.afterSwaps
  );
  expect(eventsSnapshot).toMatchObject({ seq: 1 });
  await expect(
    page.getByRole('navigation', { name: 'breadcrumbs' }).getByText('events', {
      exact: true,
    })
  ).toBeVisible();
  expect((await probeSnapshot(page)).lifecycle.nativeStarts).toBe(
    beforeEvents.lifecycle.nativeStarts
  );

  const beforeBack = await probeSnapshot(page);
  await page.goBack();
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);

  await expect(page.locator(`tr[data-key="${BIG_VISIBLE_KEY}"]`)).toBeVisible();
  await waitForGenerationCount(page, uniqueGenerations(beforeBack).length + 1);
  const restoredGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!restoredGeneration) throw new Error('history restore did not reopen Live');
  const restoredSnapshot = await waitForSnapshot(
    page,
    restoredGeneration,
    beforeBack.lifecycle.afterSwaps
  );
  expect(restoredSnapshot).toMatchObject({
    event: 'ro-live',
    kind: 'snapshot',
    g: restoredGeneration,
    seq: 1,
    v: 2,
  });
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              roRowModel: { rows: { key: string; cells: string[] }[] };
            }
          ).roRowModel.rows.length
      )
    )
    .toBe(600);

  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: {
          name: 'big-pod-0550',
          namespace: 'big',
          creationTimestamp: '2026-06-08T12:00:00Z',
        },
      },
      cells: ['big-pod-0550', '0/1', 'HistoryRestored', '11', '10m'],
    },
  ]);
  const delta = await waitForFrame(
    page,
    (frame) =>
      frame.kind === 'delta' &&
      frame.g === restoredGeneration &&
      frame.upsertKeys.includes(BIG_OFFSCREEN_KEY)
  );
  expect(delta).toMatchObject({ deltaBase: restoredSnapshot.rev, seq: 2 });
  await expect
    .poll(() =>
      page.evaluate((key) => {
        const rows = (
          window as unknown as {
            roRowModel: { rows: { key: string; cells: string[] }[] };
          }
        ).roRowModel.rows;
        return {
          count: rows.length,
          updated: rows.find((row) => row.key === key)?.cells.includes('HistoryRestored') ?? false,
        };
      }, BIG_OFFSCREEN_KEY)
    )
    .toEqual({ count: 600, updated: true });
  await expect(page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"]`)).toHaveCount(0);
  expect(
    await page.evaluate(
      (key) =>
        (
          window as unknown as {
            roVirtual: { scrollToKey(value: string): boolean };
          }
        ).roVirtual.scrollToKey(key),
      BIG_OFFSCREEN_KEY
    )
  ).toBe(true);
  await expect(page.locator(`tr[data-key="${BIG_OFFSCREEN_KEY}"] td`).nth(2)).toContainText(
    'HistoryRestored'
  );

  const after = await probeSnapshot(page);
  expect(after.lifecycle.nativeStarts).toBe(beforeBack.lifecycle.nativeStarts);
  expect(after.historyReloadRequests).toBe(beforeBack.historyReloadRequests);
  const restoredRequests = after.requests.slice(beforeBack.requests.length);
  expect(restoredRequests).toHaveLength(1);
  const restoredLifecycle = after.lifecycle.events.filter(
    (event) => event.order > (beforeBack.lifecycle.events.at(-1)?.order ?? 0)
  );
  const historyRestore = restoredLifecycle.find(
    (event) => event.kind === 'htmx:historyRestore'
  );
  const historyHit = restoredLifecycle.find((event) => event.kind === 'htmx:historyCacheHit');
  if (!historyHit || !historyRestore) {
    throw new Error('transition-free cache-hit lifecycle was not fully observed');
  }
  expect(historyHit.order).toBeLessThan(historyRestore.order);
  expect(new URL(restoredRequests[0].url).pathname).toBe(`${BIG_PODS}/_stream`);
  expect(restoredRequests[0]).toMatchObject({
    generation: restoredGeneration,
    location: new URL(BIG_PODS, page.url()).href,
    projectionRows: 600,
    version: '2',
  });
  expect(
    after.frames
      .slice(beforeBack.frames.length)
      .filter((frame) => frame.kind === 'snapshot' && frame.g === restoredGeneration)
  ).toHaveLength(1);
});

test('two cache-hit Backs recover each projection without native transition or hard reload', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop history serialization surface');
  test.setTimeout(60_000);
  await installLiveTransportProbe(page);
  await page.goto(BIG_PODS);
  await openLiveV2(page);

  const beforeEvents = await probeSnapshot(page);
  const eventsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_EVENTS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Events' }).click();
  expect((await eventsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
  await waitForGenerationCount(page, uniqueGenerations(beforeEvents).length + 1);
  const eventsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!eventsGeneration) throw new Error('events navigation did not reopen Live');
  const eventsSnapshot = await waitForSnapshot(
    page,
    eventsGeneration,
    beforeEvents.lifecycle.afterSwaps
  );
  expect(eventsSnapshot).toMatchObject({ seq: 1 });

  const beforeCurrentPods = await probeSnapshot(page);
  const podsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_PODS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Pods' }).click();
  expect((await podsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
  await waitForGenerationCount(page, uniqueGenerations(beforeCurrentPods).length + 1);
  const currentPodsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!currentPodsGeneration) throw new Error('pods navigation did not reopen Live');
  const currentPodsSnapshot = await waitForSnapshot(
    page,
    currentPodsGeneration,
    beforeCurrentPods.lifecycle.afterSwaps
  );
  expect(currentPodsSnapshot).toMatchObject({ seq: 1 });
  expect((await probeSnapshot(page)).lifecycle.nativeStarts).toBe(
    beforeCurrentPods.lifecycle.nativeStarts
  );

  const beforeBacks = await probeSnapshot(page);
  const listTraffic: { kind: 'stream' | 'table'; url: string }[] = [];
  const captureListTraffic = (request: PlaywrightRequest): void => {
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/_stream')) {
      listTraffic.push({ kind: 'stream', url: request.url() });
    } else if (pathname.endsWith('/_table')) {
      listTraffic.push({ kind: 'table', url: request.url() });
    }
  };
  page.on('request', captureListTraffic);

  try {
    const generationsBefore = uniqueGenerations(beforeBacks).length;
    await page.evaluate(() => window.history.back());
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
    await expect(
      page.getByRole('navigation', { name: 'breadcrumbs' }).getByText('events', {
        exact: true,
      })
    ).toBeVisible();
    await waitForGenerationCount(page, generationsBefore + 1);
    const restoredEventsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
    if (!restoredEventsGeneration) throw new Error('first cache hit did not reopen Events Live');
    expect(
      await waitForSnapshot(
        page,
        restoredEventsGeneration,
        beforeBacks.lifecycle.afterSwaps
      )
    ).toMatchObject({ seq: 1 });

    const afterFirstBack = await probeSnapshot(page);
    expect(afterFirstBack.lifecycle.nativeStarts).toBe(beforeBacks.lifecycle.nativeStarts);
    expect(afterFirstBack.historyReloadRequests).toBe(beforeBacks.historyReloadRequests);

    await page.evaluate(() => window.history.back());
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
    await expect(page.locator(`tr[data-key="${BIG_VISIBLE_KEY}"]`)).toBeVisible();
    await waitForGenerationCount(page, generationsBefore + 2);
    const finalGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
    if (!finalGeneration) throw new Error('second cache hit did not reopen Pods Live');
    expect(
      await waitForSnapshot(page, finalGeneration, afterFirstBack.lifecycle.afterSwaps)
    ).toMatchObject({ seq: 1 });
    await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

    const after = await probeSnapshot(page);
    const lifecycleFloor = beforeBacks.lifecycle.events.at(-1)?.order ?? 0;
    const restoredLifecycle = after.lifecycle.events.filter(
      (event) => event.order > lifecycleFloor
    );
    expect(
      restoredLifecycle.filter((event) => event.kind === 'htmx:historyCacheHit')
    ).toHaveLength(2);
    expect(
      restoredLifecycle.filter((event) => event.kind === 'htmx:historyRestore')
    ).toHaveLength(2);
    expect(after.lifecycle.nativeStarts).toBe(beforeBacks.lifecycle.nativeStarts);
    expect(after.historyReloadRequests).toBe(beforeBacks.historyReloadRequests);
    const tables = listTraffic.filter((request) => request.kind === 'table');
    const streams = listTraffic.filter((request) => request.kind === 'stream');
    expect(tables.map((request) => new URL(request.url).pathname)).toEqual([
      `${BIG_EVENTS}/_table`,
      `${BIG_PODS}/_table`,
    ]);
    expect(streams).toHaveLength(2);
    expect(streams.map((request) => new URL(request.url).pathname)).toEqual([
      `${BIG_EVENTS}/_stream`,
      `${BIG_PODS}/_stream`,
    ]);
    expect(await liveStats(page)).toMatchObject({
      seq: 1,
      state: 'open',
    });
  } finally {
    page.off('request', captureListTraffic);
  }
});

test('a held cache-miss response stays inert when a second Back wins through one hard reload', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop history miss serialization surface');
  test.setTimeout(60_000);
  await installLiveTransportProbe(page);
  await page.goto(BIG_PODS);
  await openLiveV2(page);

  const beforeEvents = await probeSnapshot(page);
  const eventsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_EVENTS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Events' }).click();
  expect((await eventsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
  await waitForGenerationCount(page, uniqueGenerations(beforeEvents).length + 1);
  const eventsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!eventsGeneration) throw new Error('events navigation did not reopen Live');
  await waitForSnapshot(page, eventsGeneration, beforeEvents.lifecycle.afterSwaps);

  const beforeCurrentPods = await probeSnapshot(page);
  const podsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_PODS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Pods' }).click();
  expect((await podsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
  await waitForGenerationCount(page, uniqueGenerations(beforeCurrentPods).length + 1);
  const currentPodsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!currentPodsGeneration) throw new Error('pods navigation did not reopen Live');
  await waitForSnapshot(page, currentPodsGeneration, beforeCurrentPods.lifecycle.afterSwaps);
  expect((await probeSnapshot(page)).lifecycle.nativeStarts).toBe(
    beforeCurrentPods.lifecycle.nativeStarts
  );

  const removedEventsEntries = await page.evaluate((path) => {
    const raw = window.sessionStorage.getItem('htmx-history-cache');
    const cache = raw ? (JSON.parse(raw) as { url?: unknown }[]) : [];
    const next = cache.filter((item) => item.url !== path);
    window.sessionStorage.setItem('htmx-history-cache', JSON.stringify(next));
    return cache.length - next.length;
  }, BIG_EVENTS);
  expect(removedEventsEntries).toBeGreaterThan(0);

  const beforeBacks = await probeSnapshot(page);
  const lifecycleFloor = beforeBacks.lifecycle.events.at(-1)?.order ?? 0;
  const listTraffic: { kind: 'stream' | 'table'; url: string }[] = [];
  const captureListTraffic = (request: PlaywrightRequest): void => {
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/_stream')) {
      listTraffic.push({ kind: 'stream', url: request.url() });
    } else if (pathname.endsWith('/_table')) {
      listTraffic.push({ kind: 'table', url: request.url() });
    }
  };
  page.on('request', captureListTraffic);

  let releaseMissResponse!: () => void;
  const missResponseGate = new Promise<void>((resolve) => {
    releaseMissResponse = resolve;
  });
  let resolveMissRequest!: (request: PlaywrightRequest) => void;
  const missRequestStarted = new Promise<PlaywrightRequest>((resolve) => {
    resolveMissRequest = resolve;
  });
  let missHeld = false;
  const holdHistoryMiss = async (route: Route, request: PlaywrightRequest): Promise<void> => {
    if (
      !missHeld &&
      new URL(request.url()).pathname === BIG_EVENTS &&
      request.headers()['hx-history-restore-request'] === 'true'
    ) {
      missHeld = true;
      resolveMissRequest(request);
      await missResponseGate;
    }
    await route.continue();
  };
  await page.route('**/*', holdHistoryMiss);

  try {
    await page.evaluate(() => {
      (
        window as unknown as {
          __liveV2Probe: LiveTransportProbe;
        }
      ).__liveV2Probe.holdNextHistoryReload();
      window.history.back();
    });
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
    const missRequest = await missRequestStarted;
    expect(missRequest.headers()['hx-history-restore-request']).toBe('true');
    await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);
    expect(listTraffic).toEqual([]);
    expect(
      (await probeSnapshot(page)).lifecycle.events.filter(
        (event) => event.order > lifecycleFloor && event.kind === 'htmx:historyCacheMiss'
      )
    ).toHaveLength(1);

    // The first Back now owns a real, still-unsettled cache-miss XHR. A second
    // Back targets the cached Pods entry and must be cancelled at its early hit
    // event, leaving one hard reload as the only possible winner.
    await page.evaluate(() => window.history.back());
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
    await expect
      .poll(async () => (await probeSnapshot(page)).historyReloadBlocked, {
        timeout: 5_000,
      })
      .toBe(true);
    const serialized = await probeSnapshot(page);
    expect(
      serialized.lifecycle.events.filter(
        (event) => event.order > lifecycleFloor && event.kind === 'htmx:historyCacheHit'
      )
    ).toHaveLength(1);
    expect(serialized.historyReloadRequests).toBe(1);
    expect(listTraffic).toEqual([]);

    // Let the obsolete Events response finish while the native reload remains
    // held. HTMX still performs MissLoad's swap even when prevented, so this is
    // a real proof that the reload gate, not event cancellation, keeps the
    // stale body from issuing its window-recovery `_table` or opening Live.
    const staleResponse = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === BIG_EVENTS &&
        response.request().headers()['hx-history-restore-request'] === 'true',
      { timeout: 15_000 }
    );
    releaseMissResponse();
    expect((await staleResponse).status()).toBe(200);
    await expect(
      page.getByRole('navigation', { name: 'breadcrumbs' }).getByText('events', {
        exact: true,
      })
    ).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(BIG_PODS);
    expect((await probeSnapshot(page)).historyReloadBlocked).toBe(true);
    expect(listTraffic).toEqual([]);
    expect((await probeSnapshot(page)).requests).toHaveLength(beforeBacks.requests.length);
    await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);

    await page.unroute('**/*', holdHistoryMiss);
    const reloaded = page.waitForEvent('domcontentloaded', { timeout: 15_000 });
    await page.evaluate(() => {
      window.setTimeout(() => {
        (
          window as unknown as {
            __liveV2Probe: LiveTransportProbe;
          }
        ).__liveV2Probe.releaseHistoryReload();
      }, 0);
    });
    await reloaded;
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
    await waitForGenerationCount(page, 1);
    const finalGeneration = uniqueGenerations(await probeSnapshot(page))[0];
    if (!finalGeneration) throw new Error('hard reload did not reopen canonical Live');
    const finalSnapshot = await waitForSnapshot(page, finalGeneration, 0);
    expect(finalSnapshot).toMatchObject({
      event: 'ro-live',
      g: finalGeneration,
      kind: 'snapshot',
      seq: 1,
      v: 2,
    });
    const finalProbe = await probeSnapshot(page);
    expect(finalProbe.requests).toHaveLength(1);
    expect(finalProbe.frames.filter((frame) => frame.kind === 'snapshot')).toHaveLength(1);
    const tables = listTraffic.filter((request) => request.kind === 'table');
    const streams = listTraffic.filter((request) => request.kind === 'stream');
    expect(tables).toEqual([]);
    expect(streams).toHaveLength(1);
    expect(new URL(streams[0].url).pathname).toBe(`${BIG_PODS}/_stream`);
  } finally {
    releaseMissResponse();
    await page.unroute('**/*', holdHistoryMiss).catch(() => {});
    page.off('request', captureListTraffic);
  }
});

test('a transition-free Back restores the sorted Pods projection without a hard reload', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'desktop normal/history serialization surface');
  test.setTimeout(60_000);
  await installLiveTransportProbe(page);
  await page.goto(`${BIG_PODS}?sort=Name`);
  await openLiveV2(page);

  const beforeEvents = await probeSnapshot(page);
  const eventsResponsePromise = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === BIG_EVENTS &&
      response.request().headers()['hx-request'] === 'true',
    { timeout: 15_000 }
  );
  await page.locator('.ro-sidebar a', { hasText: 'Events' }).click();
  expect((await eventsResponsePromise).status()).toBe(200);
  await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_EVENTS);
  await waitForGenerationCount(page, uniqueGenerations(beforeEvents).length + 1);
  const eventsGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
  if (!eventsGeneration) throw new Error('events navigation did not reopen Live');
  await waitForSnapshot(page, eventsGeneration, beforeEvents.lifecycle.afterSwaps);
  expect((await probeSnapshot(page)).lifecycle.nativeStarts).toBe(
    beforeEvents.lifecycle.nativeStarts
  );

  const beforeBack = await probeSnapshot(page);
  const lifecycleFloor = beforeBack.lifecycle.events.at(-1)?.order ?? 0;
  const listTraffic: { kind: 'stream' | 'table'; url: string }[] = [];
  const captureListTraffic = (request: PlaywrightRequest): void => {
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/_stream')) listTraffic.push({ kind: 'stream', url: request.url() });
    if (pathname.endsWith('/_table')) listTraffic.push({ kind: 'table', url: request.url() });
  };
  page.on('request', captureListTraffic);

  try {
    await page.goBack();
    await expect.poll(() => new URL(page.url()).pathname).toBe(BIG_PODS);
    await expect.poll(() => new URL(page.url()).searchParams.get('sort')).toBe('Name');
    await expect(page.locator(`tr[data-key="${BIG_VISIBLE_KEY}"]`)).toBeVisible();
    await waitForGenerationCount(page, uniqueGenerations(beforeBack).length + 1);
    const finalGeneration = uniqueGenerations(await probeSnapshot(page)).at(-1);
    if (!finalGeneration) throw new Error('cache hit did not reopen sorted Pods Live');
    const finalSnapshot = await waitForSnapshot(
      page,
      finalGeneration,
      beforeBack.lifecycle.afterSwaps
    );
    expect(finalSnapshot).toMatchObject({
      event: 'ro-live',
      g: finalGeneration,
      kind: 'snapshot',
      seq: 1,
      v: 2,
    });
    await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

    const after = await probeSnapshot(page);
    const restoredLifecycle = after.lifecycle.events.filter(
      (event) => event.order > lifecycleFloor
    );
    expect(
      restoredLifecycle.filter((event) => event.kind === 'htmx:historyCacheHit')
    ).toHaveLength(1);
    expect(
      restoredLifecycle.filter((event) => event.kind === 'htmx:historyRestore')
    ).toHaveLength(1);
    expect(after.lifecycle.nativeStarts).toBe(beforeBack.lifecycle.nativeStarts);
    expect(after.historyReloadRequests).toBe(beforeBack.historyReloadRequests);
    const tables = listTraffic.filter((request) => request.kind === 'table');
    expect(tables).toHaveLength(1);
    expect(new URL(tables[0].url).pathname).toBe(`${BIG_PODS}/_table`);
    expect(new URL(tables[0].url).searchParams.get('sort')).toBe('Name');
    const streams = listTraffic.filter((request) => request.kind === 'stream');
    expect(streams).toHaveLength(1);
    expect(new URL(streams[0].url).pathname).toBe(`${BIG_PODS}/_stream`);
    expect(new URL(streams[0].url).searchParams.get('sort')).toBe('Name');
  } finally {
    page.off('request', captureListTraffic);
  }
});
