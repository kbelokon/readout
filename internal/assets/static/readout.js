"use strict";
(() => {
  // internal/assets/src/js/htmx-config.ts
  if (typeof htmx !== "undefined") {
    htmx.config.globalViewTransitions = !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  // internal/assets/src/js/filters-parse.ts
  var goFieldWhitespaceRun = /\p{White_Space}+/gu;
  var goFieldWhitespaceEdges = /^\p{White_Space}+|\p{White_Space}+$/gu;
  function trimFilterWhitespace(s) {
    return (s || "").replace(goFieldWhitespaceEdges, "");
  }
  function normalizeFieldWhitespace(s) {
    return trimFilterWhitespace((s || "").replace(goFieldWhitespaceRun, " "));
  }
  function normalizeFieldName(s) {
    return normalizeFieldWhitespace((s || "").toLowerCase().replace(/-/g, " "));
  }
  function fieldSuggestionText(label) {
    return normalizeFieldName(label).replace(/ /g, "-");
  }
  function splitFilterDraft(s) {
    const operator = /!=|[:<>]/.exec(s);
    if (!operator) {
      return null;
    }
    const op = operator[0];
    return {
      field: trimFilterWhitespace(s.slice(0, operator.index)),
      op,
      value: s.slice(operator.index + op.length)
    };
  }
  function hasModelColumn(fields, normName) {
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === normName);
  }
  function filterSuggestionFields(fields) {
    const out = [];
    fields.forEach((f) => {
      if (!f.hint) {
        return;
      }
      const norm = normalizeFieldName(f.label);
      if (norm === "cpu" || norm === "memory") {
        return;
      }
      out.push({ text: f.name, hint: f.hint });
    });
    out.push({ text: "label", hint: "key=value" });
    if (hasModelColumn(fields, "cpu usage")) {
      out.push({ text: "cpu", hint: "quantity" });
    }
    if (hasModelColumn(fields, "memory usage")) {
      out.push({ text: "memory", hint: "quantity" });
    }
    return out;
  }
  function filterFieldKnown(fields, field) {
    const want = normalizeFieldName(field);
    if (!want) {
      return false;
    }
    if (want === "label") {
      return true;
    }
    if (want === "cpu" || want === "memory") {
      return hasModelColumn(fields, `${want} usage`);
    }
    return fields.some((f) => !!f.hint && normalizeFieldName(f.label) === want);
  }
  function fieldColumnIndex(fields, field) {
    let want = normalizeFieldName(field);
    if (want === "cpu" || want === "memory") {
      want += " usage";
    }
    for (let i = 0; i < fields.length; i++) {
      const f = fields[i];
      if (f.hint && normalizeFieldName(f.label) === want) {
        return i;
      }
    }
    return -1;
  }
  function rankFieldSuggestions(fields, draft) {
    const q = normalizeFieldName(draft);
    const ranked = [[], []];
    filterSuggestionFields(fields).forEach((field) => {
      const matchAt = normalizeFieldName(field.text).indexOf(q);
      if (matchAt >= 0) {
        ranked[matchAt === 0 ? 0 : 1].push(field);
      }
    });
    return [...ranked[0], ...ranked[1]].map((f) => ({
      label: f.text,
      hint: f.hint,
      insert: `${f.text}:`,
      kind: "field"
    }));
  }
  function rankValueSuggestions(fields, rows, split) {
    const idx = fieldColumnIndex(fields, split.field);
    if (idx < 0) {
      return [];
    }
    const freq = /* @__PURE__ */ new Map();
    rows.forEach((row) => {
      const v = row.cells[idx];
      if (v) {
        freq.set(v, (freq.get(v) || 0) + 1);
      }
    });
    const typed = trimFilterWhitespace(split.value).toLowerCase();
    const entries = Array.from(freq.entries()).filter(
      ([v]) => v.toLowerCase().indexOf(typed) !== -1
    );
    entries.sort((a, b) => b[1] - a[1]);
    return entries.slice(0, 8).map(([v, n]) => ({
      label: v,
      hint: `×${n}`,
      insert: `${trimFilterWhitespace(split.field)}:${v}`,
      kind: "value"
    }));
  }
  function liveNameMatchKeys(rows, draft) {
    const text = !draft || splitFilterDraft(draft) ? "" : trimFilterWhitespace(draft).toLowerCase();
    if (!text) {
      return null;
    }
    const visible = /* @__PURE__ */ new Set();
    rows.forEach((row) => {
      if (row.name.toLowerCase().indexOf(text) !== -1) {
        visible.add(row.key);
      }
    });
    return visible;
  }
  function mergeColParams(pathname, search, owned, fields) {
    const kept = [];
    search.replace(/^\?/, "").split("&").forEach((pair) => {
      if (pair && !owned.has(pair.split("=")[0])) {
        kept.push(pair);
      }
    });
    const query = kept.concat(fields).join("&");
    return pathname + (query ? `?${query}` : "");
  }

  // internal/assets/src/js/list-etag.ts
  var LIST_CONTENT_ID = "resource-list-content";
  var ETAG_DATA_KEY = "roEtag";
  var PATH_DATA_KEY = "roEtagPath";
  function eventDetail(event) {
    return Object(event.detail);
  }
  function currentListContent() {
    return document.getElementById(LIST_CONTENT_ID);
  }
  function validETag(value) {
    return typeof value === "string" && /^(?:W\/)?"[\x21\x23-\x7e]*"$/.test(value);
  }
  function tableRequestKey(value) {
    if (typeof value !== "string" || value.length === 0) {
      return null;
    }
    try {
      const url = new URL(value, window.location.href);
      if (url.origin !== window.location.origin || !url.pathname.endsWith("/_table")) {
        return null;
      }
      return `${url.pathname}${url.search}`;
    } catch {
      return null;
    }
  }
  function headerRecord(value) {
    return Object(value);
  }
  function headerValue(headers, wanted) {
    const lowerWanted = wanted.toLowerCase();
    for (const [name, value] of Object.entries(headerRecord(headers))) {
      if (name.toLowerCase() === lowerWanted && typeof value === "string") {
        return value;
      }
    }
    return null;
  }
  function deleteHeader(headers, unwanted) {
    const record = headerRecord(headers);
    const lowerUnwanted = unwanted.toLowerCase();
    for (const name of Object.keys(record)) {
      if (name.toLowerCase() === lowerUnwanted) {
        delete record[name];
      }
    }
  }
  function responseHeader(xhr, name) {
    const candidate = Object(xhr);
    if (typeof candidate.getResponseHeader !== "function") {
      return null;
    }
    try {
      const value = candidate.getResponseHeader.call(xhr, name);
      return typeof value === "string" ? value : null;
    } catch {
      return null;
    }
  }
  function clearContentValidator(content) {
    delete content.dataset[ETAG_DATA_KEY];
    delete content.dataset[PATH_DATA_KEY];
  }
  function readContentValidator(content) {
    const etag = content.dataset[ETAG_DATA_KEY];
    const path = content.dataset[PATH_DATA_KEY];
    if (!validETag(etag) || tableRequestKey(path) !== path) {
      clearContentValidator(content);
      return null;
    }
    return { etag, path };
  }
  function writeContentValidator(content, validator) {
    clearContentValidator(content);
    content.dataset[PATH_DATA_KEY] = validator.path;
    content.dataset[ETAG_DATA_KEY] = validator.etag;
  }
  function configureListValidatorRequest(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content) {
      return;
    }
    const sourceIsContent = detail.elt === content;
    const targetIsContent = detail.target === content;
    if (!sourceIsContent && !targetIsContent) {
      return;
    }
    const headers = headerRecord(detail.headers);
    deleteHeader(headers, "If-None-Match");
    if (!sourceIsContent || detail.target !== void 0 && !targetIsContent || headerValue(headers, "RO-No-Push") !== "true" || headerValue(headers, "HX-Preloaded") === "true") {
      return;
    }
    const validator = readContentValidator(content);
    const requestKey = tableRequestKey(detail.path);
    if (!validator || requestKey !== validator.path) {
      return;
    }
    headers["If-None-Match"] = validator.etag;
  }
  function rememberListValidator(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content || detail.target !== content) {
      return;
    }
    if (detail.roLivePush === true) {
      clearContentValidator(content);
      return;
    }
    const xhr = Object(detail.xhr);
    if (xhr.status !== 200) {
      return;
    }
    const pathInfo = Object(detail.pathInfo);
    const path = tableRequestKey(pathInfo.finalRequestPath);
    const etag = responseHeader(detail.xhr, "ETag");
    if (!path || !validETag(etag)) {
      clearContentValidator(content);
      return;
    }
    writeContentValidator(content, { etag, path });
  }
  function clearListValidator() {
    const content = currentListContent();
    if (content) {
      clearContentValidator(content);
    }
  }
  function suppressListNotModified(event) {
    const detail = eventDetail(event);
    const content = currentListContent();
    const xhr = Object(detail.xhr);
    if (!content || detail.target !== content || xhr.status !== 304) {
      return false;
    }
    detail.shouldSwap = false;
    detail.isError = true;
    const validator = readContentValidator(content);
    const config = Object(detail.requestConfig);
    const pathInfo = Object(detail.pathInfo);
    if (config.elt !== content || !validator || headerValue(config.headers, "RO-No-Push") !== "true" || headerValue(config.headers, "If-None-Match") !== validator.etag || tableRequestKey(pathInfo.finalRequestPath) !== validator.path || responseHeader(detail.xhr, "ETag") !== validator.etag) {
      return false;
    }
    detail.isError = false;
    return true;
  }

  // internal/assets/src/js/live-policy.ts
  function effectivePollSeconds(mode, intervalSeconds, liveFallbackSeconds2) {
    if (intervalSeconds > 0) {
      return intervalSeconds;
    }
    return mode === "Live" ? liveFallbackSeconds2 : 0;
  }
  function refreshDelaySeconds(effectiveSeconds, failureStage) {
    const baseSeconds = Math.max(effectiveSeconds, 0);
    if (failureStage <= 1) {
      return baseSeconds;
    }
    const factor = failureStage === 2 ? 2 : 4;
    return Math.min(baseSeconds * factor, 60);
  }
  function nextFailureStage(stage) {
    return Math.min(stage + 1, 3);
  }
  function classifyStreamClose(facts) {
    if (facts.superseded) {
      return { kind: "ignore" };
    }
    switch (facts.cause) {
      case "connect-error":
      case "bad-status":
        return { kind: "fallback", banner: false, terminal: false };
      case "read-error":
      case "eof":
        return { kind: "fallback", banner: true, terminal: false };
      case "terminal-frame":
        return { kind: "fallback", banner: true, terminal: true };
    }
  }
  function shouldDiscardPush(facts) {
    if (facts.frameGeneration !== facts.currentGeneration) {
      return "stale-generation";
    }
    if (facts.liveStreamBase !== facts.openedStreamBase) {
      return "wrong-page";
    }
    if (facts.requestInFlight) {
      return "request-in-flight";
    }
    return "none";
  }

  // internal/assets/src/js/stale.ts
  var STALE_DIM_CLASS = "ro-stale";
  var staleCountdownId = null;
  function updateStaleCountdown() {
    const span = document.querySelector(".ro-stale-banner [data-stale-countdown]");
    if (!span) {
      return;
    }
    const nextAt = refreshNextAtMs();
    if (!nextAt) {
      span.textContent = "…";
      return;
    }
    const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1e3));
    span.textContent = `${remaining}s`;
  }
  function isListRefreshEvent(event) {
    const detail = event.detail;
    if (!detail || isPreloadRequest(event)) {
      return false;
    }
    const elt = detail.elt;
    if (elt && elt.id === "resource-list-content") {
      return true;
    }
    const target = detail.target;
    return !!target && target.id === "resource-list-content";
  }
  function markListStale() {
    const content = document.getElementById("resource-list-content");
    if (content) {
      content.classList.add(STALE_DIM_CLASS);
    }
    const banner = document.querySelector(".ro-stale-banner");
    if (!banner) {
      return;
    }
    banner.hidden = false;
    if (staleCountdownId === null) {
      staleCountdownId = window.setInterval(updateStaleCountdown, 1e3);
    }
    updateStaleCountdown();
  }
  function clearListStale() {
    const content = document.getElementById("resource-list-content");
    if (content) {
      content.classList.remove(STALE_DIM_CLASS);
    }
    const banner = document.querySelector(".ro-stale-banner");
    if (banner) {
      banner.hidden = true;
    }
    if (staleCountdownId !== null) {
      window.clearInterval(staleCountdownId);
      staleCountdownId = null;
    }
  }
  document.addEventListener("htmx:responseError", (event) => {
    if (isListRefreshEvent(event)) {
      noteRefreshFailure();
      markListStale();
    }
  });
  document.addEventListener("htmx:sendError", (event) => {
    if (isListRefreshEvent(event)) {
      noteRefreshFailure();
      markListStale();
    }
  });

  // internal/assets/src/js/live.ts
  function getHtmx() {
    return window.htmx;
  }
  var liveState = {
    status: "idle",
    // 'idle' | 'connecting' | 'open' | 'fallback' | 'hidden'
    abort: null,
    // AbortController of the current stream fetch
    gen: "",
    // the minted generation every frame must echo (string compare)
    streamPath: ""
    // the stream URL sans ?g= -- the page/params identity
  };
  var liveDiscards = 0;
  var liveFallbackSecs = 0;
  function liveFallbackSeconds() {
    return liveFallbackSecs;
  }
  function liveSupported() {
    const content = document.getElementById("resource-list-content");
    if (content?.dataset.liveUrl !== "location") {
      return false;
    }
    const option = document.querySelector(
      '[data-ro-action="set-refresh"][data-ro-interval="Live"]'
    );
    return !!option && !option.disabled;
  }
  function liveStreamBase() {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, "")}/_stream${u.search}`;
  }
  function liveTeardown() {
    const ctrl = liveState.abort;
    liveState.abort = null;
    liveFallbackSecs = 0;
    if (ctrl) {
      ctrl.abort();
    }
  }
  function liveEngageFallback(banner) {
    liveTeardown();
    liveState.status = "fallback";
    liveFallbackSecs = document.getElementById("resource-list-content") ? 5 : 0;
    scheduleRefreshTick();
    if (banner) {
      markListStale();
    }
  }
  function liveOpen(base) {
    liveTeardown();
    liveState.streamPath = base;
    if (!base) {
      liveEngageFallback(false);
      return;
    }
    liveState.status = "connecting";
    liveState.gen = window.crypto.getRandomValues(new Uint32Array(4)).toString();
    const ctrl = new AbortController();
    liveState.abort = ctrl;
    const separator = base.includes("?") ? "&" : "?";
    const url = `${base}${separator}g=${liveState.gen}`;
    scheduleRefreshTick();
    void liveConnect(url, ctrl);
  }
  async function liveConnect(url, ctrl) {
    let res;
    try {
      res = await fetch(url, { signal: ctrl.signal });
    } catch {
      applyClose({ superseded: liveState.abort !== ctrl, cause: "connect-error" });
      return;
    }
    if (liveState.abort !== ctrl) {
      return;
    }
    if (res.status !== 200 || !res.body) {
      applyClose({ superseded: false, cause: "bad-status" });
      return;
    }
    liveState.status = "open";
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    let eventName = null;
    let dataLines = [];
    const readAndHandleChunk = async () => {
      const chunk = await reader.read();
      if (liveState.abort !== ctrl) {
        return;
      }
      const value = chunk.value;
      buffered += decoder.decode(value.subarray(), { stream: true });
      const completeLines = buffered.split("\n");
      buffered = completeLines.pop();
      for (const rawLine of completeLines) {
        const line = rawLine === "\r" ? "" : rawLine;
        if (line.length === 0) {
          liveHandleFrame(eventName, dataLines.join("\n"));
          eventName = null;
          dataLines = [];
          if (liveState.abort !== ctrl) {
            return;
          }
        } else if (line.startsWith("event:")) {
          eventName = line.slice(6).trim();
        } else if (line.startsWith("data:")) {
          dataLines.push(line.slice(5));
        }
      }
      return value;
    };
    try {
      while (await readAndHandleChunk()) {
      }
    } catch {
    }
    if (liveState.abort !== ctrl) {
      return;
    }
    liveEngageFallback(true);
  }
  function applyClose(facts) {
    const action = classifyStreamClose(facts);
    if (action.kind === "ignore") {
      return;
    }
    liveEngageFallback(action.banner);
  }
  function liveHandleFrame(name, text) {
    let parsed;
    try {
      parsed = JSON.parse(text);
    } catch {
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return;
    }
    const payload = parsed;
    if (name === "ro-terminal") {
      applyClose({ superseded: false, cause: "terminal-frame" });
      return;
    }
    if (name !== "ro-table") {
      return;
    }
    if (typeof payload.g !== "string" || typeof payload.html !== "string") {
      return;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    const reason = shouldDiscardPush({
      frameGeneration: payload.g,
      currentGeneration: liveState.gen,
      // The Go bundle contract pins the literal page comparison below. Feed
      // equal page identities here so the policy supplies the first and third
      // ordered gates (generation, then request); the literal supplies the
      // page gate between them without evaluating it twice.
      liveStreamBase: liveState.streamPath,
      openedStreamBase: liveState.streamPath,
      requestInFlight: userListRequestsInFlight.size > 0 || containerListRequestsInFlight.size > 0
    });
    if (reason === "stale-generation" || liveStreamBase() !== liveState.streamPath || reason === "request-in-flight") {
      liveDiscards += 1;
      return;
    }
    liveMorph(payload.html);
  }
  function liveMorph(html) {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx();
    if (!content || !htmx2 || typeof htmx2.swap !== "function") {
      return;
    }
    clearListValidator();
    htmx2.swap(
      content,
      html,
      { swapStyle: "morph" },
      {
        contextElement: content,
        eventInfo: { target: content, roLivePush: true }
      }
    );
  }
  function liveOnListSwap(event) {
    const detail = event.detail;
    if (detail?.roLivePush) {
      return;
    }
    if (liveState.status !== "open" && liveState.status !== "connecting") {
      return;
    }
    let base = liveStreamBase();
    const requestPath = detail?.pathInfo?.finalRequestPath || detail?.pathInfo?.requestPath;
    if (typeof requestPath === "string") {
      const queryStart = requestPath.indexOf("?");
      const pathname = queryStart === -1 ? requestPath : requestPath.slice(0, queryStart);
      if (pathname.endsWith("/_table")) {
        const query = queryStart === -1 ? "" : requestPath.slice(queryStart);
        base = `${pathname.slice(0, -"/_table".length)}/_stream${query}`;
      }
    }
    liveOpen(base);
  }
  function liveApply(force) {
    if (refreshMode() !== "Live") {
      liveTeardown();
      liveState.status = "idle";
      liveState.streamPath = "";
      return;
    }
    const base = liveSupported() ? liveStreamBase() : "";
    if (!force && base === liveState.streamPath && liveState.status !== "idle") {
      return;
    }
    liveOpen(base);
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      if (liveState.status === "open" || liveState.status === "connecting") {
        liveTeardown();
        liveState.status = "hidden";
      }
      return;
    }
    if (liveState.status === "hidden" && refreshMode() === "Live") {
      liveOpen(liveSupported() ? liveStreamBase() : "");
    }
  });
  window.roLive = {
    discards() {
      return liveDiscards;
    }
  };

  // internal/assets/src/js/prefs.ts
  var PREFS_COOKIE = "ro_prefs";
  var PREFS_VERSION_PREFIX = "v1.";
  var PREFS_MAX_ENCODED = 3072;
  var PREFS_COOKIE_MAX_AGE = 31536e3;
  var REFRESH_KEY = "roRefresh";
  function b64urlEncodeUTF8(text) {
    const bytes = new TextEncoder().encode(text);
    const bin = Array.from(bytes, (byte) => String.fromCharCode(byte)).join("");
    return btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
  }
  function b64urlDecodeUTF8(encoded) {
    const compact = encoded.replaceAll("\r", "").replaceAll("\n", "");
    if (!/^[A-Za-z0-9_-]*$/.test(compact)) {
      throw new TypeError();
    }
    const b64 = compact.replaceAll("-", "+").replaceAll("_", "/");
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (char) => char.charCodeAt(0));
    return new TextDecoder("utf-8", { ignoreBOM: true }).decode(bytes);
  }
  function isRecord(value) {
    return typeof value === "object" && value !== null && !Array.isArray(value);
  }
  function setOwnNamespace(ns, cluster, namespace) {
    Object.defineProperty(ns, cluster, {
      value: namespace,
      enumerable: true,
      configurable: true,
      writable: true
    });
  }
  function compareUTF8(left, right) {
    const mismatch = left.subarray(0, right.length).findIndex((byte, index) => byte !== right[index]);
    if (mismatch !== -1) {
      return left[mismatch] - right[mismatch];
    }
    return left.length - right.length;
  }
  function stringifyPrefsJSON(value) {
    const encoded = JSON.stringify(value);
    return encoded.replaceAll("\u2028", "\\u2028").replaceAll("\u2029", "\\u2029");
  }
  function canonicalNamespaceJSON(ns) {
    const encoder = new TextEncoder();
    const entries = Object.entries(ns).map(([cluster, namespace]) => ({
      cluster,
      namespace,
      encodedCluster: encoder.encode(cluster)
    }));
    entries.sort((left, right) => compareUTF8(left.encodedCluster, right.encodedCluster));
    return `{${entries.map(
      ({ cluster, namespace }) => `${stringifyPrefsJSON(cluster)}:${stringifyPrefsJSON(namespace)}`
    ).join(",")}}`;
  }
  function decodePrefsValue(value) {
    const empty = { kinds: [], refresh: "", ns: {} };
    if (!value?.startsWith(PREFS_VERSION_PREFIX)) {
      return { prefs: empty, ok: false };
    }
    const payload = value.slice(PREFS_VERSION_PREFIX.length);
    try {
      const decoded = JSON.parse(b64urlDecodeUTF8(payload));
      if (!isRecord(decoded)) {
        return { prefs: empty, ok: false };
      }
      const kinds = [];
      if (Array.isArray(decoded.kinds)) {
        decoded.kinds.forEach((raw) => {
          if (!isRecord(raw)) {
            return;
          }
          const e = raw;
          if (typeof e.k !== "string") {
            return;
          }
          const entry = { k: e.k };
          if (typeof e.sort === "string") {
            entry.sort = e.sort;
          }
          if (Array.isArray(e.hide) && e.hide.every((name) => typeof name === "string")) {
            entry.hide = e.hide;
          }
          kinds.push(entry);
        });
      }
      const ns = {};
      if (isRecord(decoded.ns)) {
        Object.entries(decoded.ns).forEach(([cluster, namespace]) => {
          if (typeof namespace === "string") {
            setOwnNamespace(ns, cluster, namespace);
          }
        });
      }
      return {
        prefs: {
          kinds,
          refresh: typeof decoded.refresh === "string" ? decoded.refresh : "",
          ns
        },
        ok: true
      };
    } catch (_e) {
      return { prefs: empty, ok: false };
    }
  }
  function encodePrefsCandidate(kinds, refresh, ns) {
    const fields = [];
    if (kinds.length > 0) {
      fields.push(`"kinds":${stringifyPrefsJSON(kinds)}`);
    }
    if (refresh) {
      fields.push(`"refresh":${stringifyPrefsJSON(refresh)}`);
    }
    if (Object.keys(ns).length > 0) {
      fields.push(`"ns":${canonicalNamespaceJSON(ns)}`);
    }
    return PREFS_VERSION_PREFIX + b64urlEncodeUTF8(`{${fields.join(",")}}`);
  }
  function encodePrefsValue(prefs) {
    const kinds = prefs.kinds ?? [];
    const refresh = prefs.refresh ?? "";
    const ns = prefs.ns ?? {};
    const evictionBoundary = PREFS_MAX_ENCODED + 1;
    let value = encodePrefsCandidate(kinds, refresh, ns);
    if (value.length < evictionBoundary) {
      return value;
    }
    Array.from({ length: kinds.length }).some((_, evicted) => {
      const kept = kinds.length - evicted - 1;
      value = encodePrefsCandidate(kinds.slice(0, kept), refresh, ns);
      return value.length < evictionBoundary;
    });
    return value;
  }
  function prefsCookieValue() {
    const prefix = `${PREFS_COOKIE}=`;
    return document.cookie.split("; ").find((part) => part.startsWith(prefix))?.slice(prefix.length);
  }
  function readPrefs() {
    return decodePrefsValue(prefsCookieValue()).prefs;
  }
  function writePrefs(prefs) {
    try {
      let cookie = PREFS_COOKIE + "=" + encodePrefsValue(prefs) + "; Path=/; SameSite=Lax; Max-Age=" + PREFS_COOKIE_MAX_AGE;
      if (window.location.protocol === "https:") {
        cookie += "; Secure";
      }
      document.cookie = cookie;
    } catch (_e) {
    }
  }
  function prefsTouchKind(prefs, plural) {
    const index = prefs.kinds.findIndex((entry2) => entry2.k === plural);
    const entry = index < 0 ? { k: plural } : prefs.kinds.splice(index, 1)[0];
    prefs.kinds.unshift(entry);
    return entry;
  }
  function roPrefsSetSort(plural, sort) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).sort = sort;
    writePrefs(prefs);
  }
  function roPrefsSetHiddenColumns(plural, names) {
    const prefs = readPrefs();
    prefsTouchKind(prefs, plural).hide = names;
    writePrefs(prefs);
  }
  function roPrefsSetRefresh(mode) {
    const prefs = readPrefs();
    prefs.refresh = mode;
    writePrefs(prefs);
  }
  function roPrefsSetNamespace(cluster, namespace) {
    if (!cluster || !namespace) {
      return;
    }
    const prefs = readPrefs();
    setOwnNamespace(prefs.ns, cluster, namespace);
    writePrefs(prefs);
  }

  // internal/assets/src/js/refresh.ts
  function getHtmx2() {
    return window.htmx;
  }
  var refreshTimerId = null;
  var refreshNextAt = 0;
  var refreshFailureStage = 0;
  function refreshNextAtMs() {
    return refreshNextAt;
  }
  var userListRequestsInFlight = /* @__PURE__ */ new Set();
  var containerListRequestsInFlight = /* @__PURE__ */ new Set();
  function requestDetail(event) {
    return Object(event.detail);
  }
  function pruneSettledListRequests(requests) {
    requests.forEach((xhr) => {
      if (xhr.readyState === 4 || xhr.readyState === 0) {
        requests.delete(xhr);
      }
    });
  }
  function isPreloadRequest(event) {
    const config = Object(requestDetail(event).requestConfig);
    const headers = Object(config.headers);
    return headers["HX-Preloaded"] === "true";
  }
  function isUserListRequest(event) {
    const detail = requestDetail(event);
    return detail.elt instanceof Element && detail.target instanceof Element && detail.target.id === "resource-list-content" && !isPreloadRequest(event);
  }
  function handleRefreshConfigRequest(event) {
    const detail = requestDetail(event);
    const content = document.getElementById("resource-list-content");
    const sourceIsContent = content !== null && detail.elt === content;
    const targetIsContent = content !== null && detail.target === content;
    if (sourceIsContent || targetIsContent) {
      const headers = Object(detail.headers);
      for (const name of Object.keys(headers)) {
        if (name.toLowerCase() === "ro-no-push") {
          delete headers[name];
        }
      }
      if (sourceIsContent && (detail.target === void 0 || targetIsContent)) {
        headers["RO-No-Push"] = "true";
      }
    }
    configureListValidatorRequest(event);
  }
  document.addEventListener("htmx:configRequest", handleRefreshConfigRequest);
  function handleRefreshBeforeRequest(event) {
    const detail = requestDetail(event);
    const elt = Object(detail.elt);
    if (elt.id === "resource-list-content") {
      if (detail.xhr) {
        containerListRequestsInFlight.add(detail.xhr);
      }
      return;
    }
    if (!isUserListRequest(event)) {
      return;
    }
    if (detail.xhr) {
      userListRequestsInFlight.add(detail.xhr);
    }
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx2();
    if (content && htmx2) {
      htmx2.trigger(content, "htmx:abort");
    }
  }
  document.addEventListener("htmx:beforeRequest", handleRefreshBeforeRequest);
  function handleRefreshAfterRequest(event) {
    const xhr = requestDetail(event).xhr;
    userListRequestsInFlight.delete(xhr);
    containerListRequestsInFlight.delete(xhr);
  }
  document.addEventListener("htmx:afterRequest", handleRefreshAfterRequest);
  function parseRefreshSeconds(mode) {
    if (typeof mode !== "string") {
      return 0;
    }
    const unsigned = mode.startsWith("+") ? mode.slice(1) : mode;
    const seconds = Array.from(unsigned).reduce((value, char) => {
      const digit = char.charCodeAt(0) - 48;
      return digit >= 0 && digit <= 9 ? value * 10 + digit : Number.NaN;
    }, 0);
    return Number.isSafeInteger(seconds) ? seconds : 0;
  }
  function refreshMode() {
    const stored = readPrefs().refresh;
    if (stored) {
      return stored;
    }
    let legacy = null;
    try {
      legacy = window.localStorage.getItem(REFRESH_KEY);
    } catch {
    }
    if (!legacy) {
      return "";
    }
    const secs = parseRefreshSeconds(legacy);
    const mode = secs > 0 ? String(secs) : "Off";
    roPrefsSetRefresh(mode);
    return mode;
  }
  function listTableURL() {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, "")}/_table${u.search}`;
  }
  function requestListRefresh() {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx2();
    if (!content || !htmx2) {
      return;
    }
    if (content.dataset.liveUrl === "location") {
      const request = htmx2.ajax("GET", listTableURL(), { source: content });
      if (request && typeof request.catch === "function") {
        request.catch(() => {
        });
      }
    } else {
      htmx2.trigger(content, "ro:refresh");
    }
  }
  window.requestListRefresh = requestListRefresh;
  function fireRefresh() {
    if (document.hidden) {
      return;
    }
    pruneSettledListRequests(userListRequestsInFlight);
    pruneSettledListRequests(containerListRequestsInFlight);
    if (userListRequestsInFlight.size > 0) {
      return;
    }
    if (containerListRequestsInFlight.size > 0) {
      return;
    }
    requestListRefresh();
  }
  function effectivePollSeconds2() {
    const mode = refreshMode();
    return effectivePollSeconds(mode, parseRefreshSeconds(mode), liveFallbackSeconds());
  }
  function refreshDelaySeconds2() {
    return refreshDelaySeconds(effectivePollSeconds2(), refreshFailureStage);
  }
  function scheduleRefreshTick() {
    if (refreshTimerId !== null) {
      window.clearTimeout(refreshTimerId);
      refreshTimerId = null;
    }
    const delay = refreshDelaySeconds2();
    if (delay <= 0) {
      refreshNextAt = 0;
      updateStaleCountdown();
      return;
    }
    const delayMs = delay * 1e3;
    refreshNextAt = Date.now() + delayMs;
    refreshTimerId = window.setTimeout(() => {
      refreshTimerId = null;
      scheduleRefreshTick();
      fireRefresh();
    }, delayMs);
    updateStaleCountdown();
  }
  function applyRefresh() {
    refreshFailureStage = 0;
    scheduleRefreshTick();
  }
  function syncRefreshUI() {
    const mode = refreshMode();
    const live = mode === "Live";
    const secs = parseRefreshSeconds(mode);
    const label = document.getElementById("refresh-label");
    if (label) {
      if (live) {
        label.textContent = "Live";
      } else if (secs > 0) {
        label.textContent = `${secs}s`;
      } else {
        label.textContent = "Off";
      }
    }
    document.querySelectorAll('[data-ro-action="set-refresh"]').forEach((opt) => {
      const value = opt.dataset.roInterval;
      opt.classList.toggle(
        "is-active",
        value !== void 0 && (live ? value === "Live" : value !== "Live" && parseRefreshSeconds(value) === secs)
      );
    });
    const dropdown = document.getElementById("refresh-dropdown");
    if (dropdown) {
      dropdown.classList.toggle("refresh-on", live || secs > 0);
    }
  }
  function noteRefreshFailure() {
    refreshFailureStage = nextFailureStage(refreshFailureStage);
    scheduleRefreshTick();
  }
  function noteRefreshRecovery() {
    if (refreshFailureStage === 0) {
      return;
    }
    refreshFailureStage = 0;
    scheduleRefreshTick();
    const toast = window.roToast;
    if (typeof toast === "function") {
      toast("Refresh resumed");
    }
  }
  function handleRefreshVisibilityChange() {
    if (!document.hidden && effectivePollSeconds2() > 0) {
      fireRefresh();
    }
  }
  document.addEventListener("visibilitychange", handleRefreshVisibilityChange);
  var refreshBindings = [
    // Stale-banner retry: re-fire the (read-only) auto-refresh GET on
    // #resource-list-content through the shared refresh path (the v2 loop derives
    // the `_table` URL from location.href at click time; the v1 multi-type
    // container triggers its baked ro:refresh). On success the morph swaps fresh
    // rows and the afterSwap handler clears the stale dim + re-hides the banner;
    // on another failure the responseError handler keeps it stale. An in-flight
    // container request (a HUNG tick is exactly the state this button exists for)
    // is aborted first -- issuing a second container request would make htmx
    // QUEUE it, and a queued request replays on the next htmx:abort with its stale
    // queue-time URL (no queue may ever form). Pure DOM, GET-only -- the
    // read-only floor is untouched.
    {
      event: "click",
      selector: '[data-ro-action="retry"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const content = document.getElementById("resource-list-content");
        const htmx2 = getHtmx2();
        if (content && htmx2) {
          htmx2.trigger(content, "htmx:abort");
        }
        requestListRefresh();
        return true;
      }
    },
    // Auto-refresh interval option (navbar #refresh-dropdown): persist the chosen
    // mode in the ro_prefs cookie, re-arm the poll, and reflect it in the
    // control. The Live option persists the literal 'Live' and rides
    // the same path: liveApply opens/tears down the stream, applyRefresh then arms
    // the poll chain per the EFFECTIVE seconds (0 while a stream is riding). A
    // disabled Live option (multi-type/multi-cluster page) never fires (the
    // browser suppresses clicks on disabled buttons). The dropdown opens through
    // CSS hover/focus, so there is no open/close handler here -- only the
    // selection. Kept its early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="set-refresh"]',
      stop: true,
      handler: (event, matched) => {
        const option = matched;
        if (option.dataset.roInterval === "Live") {
          roPrefsSetRefresh("Live");
        } else {
          const interval = parseRefreshSeconds(option.dataset.roInterval);
          roPrefsSetRefresh(interval > 0 ? String(interval) : "Off");
        }
        liveApply(true);
        syncRefreshUI();
        applyRefresh();
        option.blur();
        event.preventDefault();
        return true;
      }
    }
  ];

  // internal/assets/src/js/row-selection.ts
  var rowSelection = /* @__PURE__ */ new Map();
  var rowFocusKey = null;
  function reapplyRowState() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    let focusedRow = null;
    content.querySelectorAll("tr[data-key]").forEach((tr) => {
      const row = tr;
      row.classList.toggle("is-selected", rowSelection.has(row.dataset.key));
      const focused = row.dataset.key === rowFocusKey;
      row.classList.toggle("kfocus", focused);
      if (focused) {
        focusedRow = row;
      }
    });
    content.querySelectorAll(".ro-table-wrap").forEach((wrap) => {
      const fr = focusedRow;
      if (fr?.id && wrap.contains(fr)) {
        wrap.setAttribute("aria-activedescendant", fr.id);
      } else {
        wrap.removeAttribute("aria-activedescendant");
      }
    });
  }
  function lastKeySegment(key) {
    const parts = (key || "").split("/");
    return parts[parts.length - 1] || "";
  }
  function rowSelectionEntry(key) {
    const content = document.getElementById("resource-list-content");
    let entry = null;
    if (content) {
      content.querySelectorAll("tr[data-key]").forEach((tr) => {
        const row = tr;
        if (row.dataset.key === key) {
          entry = { name: row.dataset.name || lastKeySegment(key) };
        }
      });
    }
    return entry || { name: lastKeySegment(key) };
  }
  function setRowSelected(key, on) {
    if (on) {
      rowSelection.set(key, rowSelectionEntry(key));
    } else {
      rowSelection.delete(key);
    }
    reapplyRowState();
    updateBulkBar();
  }
  function clearRowState() {
    rowSelection.clear();
    rowFocusKey = null;
    reapplyRowState();
    updateBulkBar();
  }
  window.roRowState = {
    setSelected: setRowSelected,
    setFocus(key) {
      rowFocusKey = key || null;
      reapplyRowState();
    },
    // focusedKey is the j/k focus seam the windowed walker (virtualizeMoveFocus,
    // still in legacy.js) reads across the module boundary -- the focused row can
    // be detached off-window, so the store (not the DOM kfocus class) is the
    // truth there. Also a debug sim the console can poll.
    focusedKey() {
      return rowFocusKey;
    },
    clear: clearRowState,
    selectedKeys() {
      return Array.from(rowSelection.keys());
    },
    // selectedEntries feeds the bulk actions: Copy names reads .name, and the
    // bulk Download-YAML builds its names list from .key/.name.
    selectedEntries() {
      return Array.from(rowSelection, ([key, entry]) => ({ key, name: entry.name }));
    }
  };
  var BULK_NAMES_MAX = 100;
  var bulkOverCapToasted;
  function updateBulkBar() {
    const bar = document.getElementById("ro-bulkbar");
    if (!bar) {
      return;
    }
    const count = rowSelection.size;
    const label = document.getElementById("ro-bulk-count");
    if (label) {
      label.textContent = `${count} selected`;
    }
    bar.classList.toggle("is-open", count > 0);
    bar.toggleAttribute("inert", count === 0);
    const download = document.getElementById("ro-bulk-download");
    if (download && bar.dataset.bulkHref) {
      const over = count > BULK_NAMES_MAX;
      download.disabled = over;
      download.title = over ? `Over the ${BULK_NAMES_MAX}-object bulk download cap` : "";
      if (over && !bulkOverCapToasted) {
        roToast(`Download refused: ${count} selected (max ${BULK_NAMES_MAX})`);
      }
      bulkOverCapToasted = over;
    }
  }
  function roToast(message) {
    const fn = window.roToast;
    if (typeof fn === "function") {
      fn(message);
    }
  }
  function roCopyText(text, done) {
    const fallback = () => {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.readOnly = true;
      ta.style.position = "fixed";
      ta.style.top = "-1000px";
      document.body.appendChild(ta);
      try {
        ta.select();
        return document.execCommand("copy");
      } catch {
        return false;
      } finally {
        ta.remove();
      }
    };
    if (navigator.clipboard?.writeText) {
      navigator.clipboard.writeText(text).then(
        () => done(true),
        () => done(fallback())
      );
      return;
    }
    done(fallback());
  }
  function toggleRowSelection(tr) {
    const key = tr.dataset.key;
    if (!key) {
      return;
    }
    if (rowSelection.has(key)) {
      rowSelection.delete(key);
    } else {
      rowSelection.set(key, { name: tr.dataset.name || lastKeySegment(key) });
    }
    reapplyRowState();
    updateBulkBar();
  }
  var rowSelectionBindings = [
    {
      event: "click",
      selector: "#resource-list-content tr[data-key]",
      handler: (event, matched) => {
        const target = event.target;
        if (target?.closest("a, button, input, select, textarea, label")) {
          return;
        }
        toggleRowSelection(matched);
      }
    }
  ];

  // internal/assets/src/js/virtualizer-math.ts
  var VIRT_BUFFER_ROWS = 12;
  function windowBounds(tbodyTop, innerHeight, rowH, visibleCount, buffer = VIRT_BUFFER_ROWS) {
    const pitch = rowH || 1;
    const n = visibleCount;
    const first = Math.floor((0 - tbodyTop) / pitch);
    const last = Math.ceil((innerHeight - tbodyTop) / pitch);
    const start = Math.min(n, Math.max(0, first - buffer));
    const end = Math.max(start, Math.min(n, last + buffer));
    return { start, end };
  }
  function spacerHeights(start, end, visibleCount, rowH) {
    return {
      top: start * rowH,
      bottom: Math.max(0, visibleCount - end) * rowH
    };
  }
  function prepareSwapSpacers(priorStart, incomingRowCount, rowH) {
    const start = Math.min(priorStart, incomingRowCount);
    return {
      top: start * rowH,
      bottom: Math.max(0, incomingRowCount - start) * rowH
    };
  }
  function rowOffsetTop(tbodyTop, index, rowH) {
    return tbodyTop + index * rowH;
  }
  function scrollAdjustToReveal(rowTop, rowH, topMin, innerHeight) {
    const topOverflow = rowTop - topMin;
    if (topOverflow < 0) {
      return topOverflow;
    }
    return Math.max(0, rowTop + rowH - innerHeight);
  }
  function clampFocusIndex(current, delta, visibleCount) {
    return Math.max(0, Math.min(visibleCount - 1, current + delta));
  }

  // internal/assets/src/js/virtualizer.ts
  var FILTER_HIDE_CLASS = "ro-row-filtered";
  function roRowModel() {
    return window.roRowModel;
  }
  function roRowState() {
    return window.roRowState;
  }
  var virtState = {
    active: false,
    rows: [],
    byKey: /* @__PURE__ */ new Map(),
    visible: [],
    rowH: 0,
    start: 0,
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    pendingRows: null,
    pendingScrollY: null
  };
  var historyRecoveryPending = null;
  function virtualizerActive() {
    return virtState.active && virtState.tbody?.isConnected === true;
  }
  function virtReset() {
    virtState.active = false;
    virtState.rows = [];
    virtState.byKey = /* @__PURE__ */ new Map();
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
  }
  function virtMakeSpacer() {
    const tr = document.createElement("tr");
    tr.className = "ro-vspacer";
    tr.setAttribute("aria-hidden", "true");
    tr.appendChild(document.createElement("td"));
    return tr;
  }
  function virtSetSpacerColspan() {
    const cols = virtState.table.querySelectorAll("thead th").length || 1;
    virtState.topSpacer.firstElementChild.colSpan = cols;
    virtState.bottomSpacer.firstElementChild.colSpan = cols;
  }
  function virtMeasureRowHeight() {
    const rendered = virtState.tbody.querySelectorAll(
      ":scope > tr[data-key]"
    );
    if (rendered.length === 0) {
      return 0;
    }
    const first = rendered[0].getBoundingClientRect();
    const last = rendered[rendered.length - 1].getBoundingClientRect();
    const pitch = (last.bottom - first.top) / rendered.length;
    return Math.max(0, pitch);
  }
  function virtFallbackRowHeight() {
    let py = 9;
    let lh = 18;
    try {
      const cs = window.getComputedStyle(document.documentElement);
      py = parseFloat(cs.getPropertyValue("--row-py")) || py;
      const cell = virtState.tbody?.querySelector("td");
      if (cell) {
        lh = parseFloat(window.getComputedStyle(cell).lineHeight) || lh;
      }
    } catch {
    }
    return py * 2 + lh + 1;
  }
  function virtApplyPins() {
    const ths = virtState.table.querySelectorAll("thead th");
    if (virtState.pinnedWidths.length !== ths.length) {
      return false;
    }
    ths.forEach((th, i) => {
      th.style.width = `${virtState.pinnedWidths[i]}px`;
    });
    virtState.table.classList.add("ro-virtualized");
    return true;
  }
  function virtPinColumns() {
    const ths = Array.from(virtState.table.querySelectorAll("thead th"));
    virtState.pinnedWidths = ths.map((th) => th.getBoundingClientRect().width);
    virtApplyPins();
  }
  function virtComputeVisible() {
    const keys = roRowModel().visibleKeys;
    virtState.visible = keys ? virtState.rows.filter((tr) => keys.has(tr.dataset.key)) : virtState.rows;
  }
  function virtRenderWindow() {
    const s = virtState;
    const tbody = s.tbody;
    const rect = tbody.getBoundingClientRect();
    const bounds = windowBounds(rect.top, window.innerHeight, s.rowH, s.visible.length);
    s.start = bounds.start;
    s.end = bounds.end;
    const heights = spacerHeights(s.start, s.end, s.visible.length, s.rowH);
    s.topSpacer.firstElementChild.style.height = `${heights.top}px`;
    s.bottomSpacer.firstElementChild.style.height = `${heights.bottom}px`;
    const slice = s.visible.slice(s.start, s.end);
    slice.forEach((tr) => {
      tr.classList.remove(FILTER_HIDE_CLASS);
    });
    tbody.replaceChildren(s.topSpacer, ...slice, s.bottomSpacer);
    reapplyRowState();
  }
  function virtBindMounts() {
    const content = document.getElementById("resource-list-content");
    const wrap = content?.querySelector(".ro-table-wrap.ro-windowed");
    const table = wrap?.querySelector("table.ro-table") ?? null;
    const tbody = table?.tBodies.item(0) ?? null;
    virtState.table = table;
    virtState.tbody = tbody;
    return tbody !== null;
  }
  function virtualizeInit() {
    const content = document.getElementById("resource-list-content");
    const wrap = content?.querySelector(".ro-table-wrap.ro-windowed");
    if (!content || !wrap) {
      virtReset();
      return;
    }
    const table = wrap.querySelector("table.ro-table");
    const tbody = table?.tBodies.item(0) ?? null;
    if (!tbody) {
      virtReset();
      return;
    }
    if (tbody.querySelector(":scope > tr.ro-vspacer")) {
      if (virtState.active && virtState.tbody === tbody) {
        return;
      }
      if (historyRecoveryPending?.content === content && historyRecoveryPending.tbody === tbody) {
        return;
      }
      virtReset();
      historyRecoveryPending = { content, tbody };
      clearListValidator();
      requestListRefresh();
      return;
    }
    const rows = Array.from(tbody.querySelectorAll(":scope > tr[data-key]"));
    if (rows.length === 0) {
      virtReset();
      return;
    }
    historyRecoveryPending = null;
    virtReset();
    virtState.table = table;
    virtState.tbody = tbody;
    virtState.rows = rows;
    virtState.byKey = new Map(rows.map((tr) => [tr.dataset.key, tr]));
    virtState.topSpacer = virtMakeSpacer();
    virtState.bottomSpacer = virtMakeSpacer();
    virtSetSpacerColspan();
    virtState.rowH = virtMeasureRowHeight() || virtFallbackRowHeight();
    virtPinColumns();
    virtState.active = true;
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtualizePrepareSwap(fragment) {
    virtState.pendingRows = null;
    virtState.pendingScrollY = null;
    const wrap = fragment.querySelector(".ro-table-wrap.ro-windowed");
    const tbody = wrap ? wrap.querySelector("table.ro-table tbody") : null;
    if (!tbody) {
      return;
    }
    const rows = Array.from(tbody.querySelectorAll(":scope > tr[data-key]"));
    if (rows.length === 0) {
      return;
    }
    virtState.pendingRows = rows;
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const priorStart = virtState.active ? virtState.start : 0;
    const heights = prepareSwapSpacers(priorStart, rows.length, rowH);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    topSpacer.firstElementChild.style.height = `${heights.top}px`;
    bottomSpacer.firstElementChild.style.height = `${heights.bottom}px`;
    tbody.replaceChildren(topSpacer, bottomSpacer);
  }
  function virtualizeAfterSwap() {
    historyRecoveryPending = null;
    const pending = virtState.pendingRows;
    virtState.pendingRows = null;
    if (!pending) {
      virtReset();
      return;
    }
    const prior = virtState.byKey;
    const wasActive = virtState.active;
    if (!virtBindMounts()) {
      virtReset();
      return;
    }
    virtState.rows = pending;
    virtState.byKey = new Map(pending.map((tr) => [tr.dataset.key, tr]));
    if (!virtState.topSpacer) {
      virtState.topSpacer = virtMakeSpacer();
      virtState.bottomSpacer = virtMakeSpacer();
    }
    virtSetSpacerColspan();
    virtState.active = true;
    if (!virtState.rowH) {
      virtState.rowH = virtFallbackRowHeight();
    }
    virtComputeVisible();
    virtRenderWindow();
    if (!wasActive) {
      const measured = virtMeasureRowHeight();
      if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
        virtState.rowH = measured;
        virtRenderWindow();
      }
    }
    if (!virtApplyPins()) {
      virtPinColumns();
    }
    if (virtState.pendingScrollY !== null && window.scrollY !== virtState.pendingScrollY) {
      window.scrollTo(0, virtState.pendingScrollY);
      virtRenderWindow();
    }
    virtState.pendingScrollY = null;
    virtFlashChangedCells(prior);
  }
  function virtFlashChangedCells(prior) {
    if (prior.size === 0 || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    virtState.tbody.querySelectorAll(":scope > tr[data-key]").forEach((tr) => {
      const old = prior.get(tr.dataset.key);
      if (!old) {
        return;
      }
      Array.from(tr.children).forEach((newCell, index) => {
        const oldCell = old.children.item(index);
        if (oldCell && newCell.tagName === "TD" && oldCell.textContent !== newCell.textContent) {
          newCell.classList.remove("ro-cell-changed");
          void newCell.offsetWidth;
          newCell.classList.add("ro-cell-changed");
        }
      });
    });
  }
  function virtualizeOnFilterChange() {
    if (!virtualizerActive() || virtState.pendingRows) {
      return;
    }
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtMoveFocus(delta) {
    const list = virtState.visible;
    if (list.length === 0) {
      return false;
    }
    const focusKey = roRowState().focusedKey();
    const current = list.findIndex((row) => row.dataset.key === focusKey);
    const next = clampFocusIndex(current, delta, list.length);
    virtualizeScrollToIndex(next);
    roRowState().setFocus(list[next].dataset.key);
    return true;
  }
  function virtualizeScrollToIndex(index) {
    const rect = virtState.tbody.getBoundingClientRect();
    const rowTop = rowOffsetTop(rect.top, index, virtState.rowH);
    const topbar = document.querySelector("header.ro-topbar");
    const topMin = topbar ? topbar.getBoundingClientRect().bottom : 0;
    const delta = scrollAdjustToReveal(rowTop, virtState.rowH, topMin, window.innerHeight);
    if (delta !== 0) {
      window.scrollBy(0, delta);
    }
    virtRenderWindow();
  }
  function virtRows() {
    return virtState.rows;
  }
  function virtVisible() {
    return virtState.visible;
  }
  function virtRowByKey(key) {
    return virtState.byKey.get(key) || null;
  }
  var virtScrollScheduled = false;
  function virtOnScroll() {
    if (!virtualizerActive()) {
      return;
    }
    const rect = virtState.tbody.getBoundingClientRect();
    const bounds = windowBounds(
      rect.top,
      window.innerHeight,
      virtState.rowH,
      virtState.visible.length
    );
    if (bounds.start !== virtState.start || bounds.end !== virtState.end) {
      virtRenderWindow();
    }
  }
  window.addEventListener(
    "scroll",
    () => {
      if (!virtState.active || virtScrollScheduled) {
        return;
      }
      virtScrollScheduled = true;
      window.requestAnimationFrame(() => {
        virtScrollScheduled = false;
        virtOnScroll();
      });
    },
    { passive: true }
  );
  window.addEventListener("resize", virtOnScroll);
  var fontReady = document.fonts?.ready;
  if (fontReady && typeof fontReady.then === "function") {
    void fontReady.then(() => {
      if (!virtualizerActive()) {
        return;
      }
      const measured = virtMeasureRowHeight();
      if (measured && Math.abs(measured - virtState.rowH) > 0.5) {
        virtState.rowH = measured;
        virtRenderWindow();
      }
    });
  }
  window.roVirtual = {
    active: virtualizerActive,
    renderedBounds() {
      return { start: virtState.start, end: virtState.end, total: virtState.visible.length };
    },
    scrollToKey(key) {
      if (!virtualizerActive()) {
        return false;
      }
      const tr = virtState.byKey.get(key);
      const index = tr ? virtState.visible.indexOf(tr) : -1;
      if (index === -1) {
        return false;
      }
      virtualizeScrollToIndex(index);
      return true;
    }
  };

  // internal/assets/src/js/filters.ts
  function getHtmx3() {
    return window.htmx;
  }
  var roRowModel2 = {
    fields: [],
    rows: [],
    visibleKeys: null
  };
  window.roRowModel = roRowModel2;
  function captureRowModel(root) {
    const table = root.querySelector("table.ro-table");
    if (!table) {
      roRowModel2.fields = [];
      roRowModel2.rows = [];
      return;
    }
    const fields = [];
    table.querySelectorAll("thead th").forEach((th) => {
      const label = normalizeFieldWhitespace(th.textContent || "");
      fields.push({
        label,
        name: fieldSuggestionText(label),
        hint: th.dataset.hint || ""
      });
    });
    const rows = [];
    table.querySelectorAll("tbody tr[data-key]").forEach((tr) => {
      const cells = [];
      tr.querySelectorAll("td").forEach((td) => {
        cells.push(trimFilterWhitespace(td.textContent || ""));
      });
      const nameLink = tr.querySelector("td.cell-name a");
      rows.push({
        key: tr.dataset.key,
        name: nameLink ? trimFilterWhitespace(nameLink.textContent || "") : cells[0] || "",
        cells
      });
    });
    roRowModel2.fields = fields;
    roRowModel2.rows = rows;
  }
  function captureRowModelFromDocument() {
    const content = document.getElementById("resource-list-content");
    if (content && document.getElementById("ro-filter-input") && !virtualizerActive()) {
      captureRowModel(content);
    }
  }
  var FILTER_HIDE_CLASS2 = "ro-row-filtered";
  function applyLiveNameFilter() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    const input = document.getElementById("ro-filter-input");
    const draft = input ? input.value : "";
    const visible = liveNameMatchKeys(roRowModel2.rows, draft);
    roRowModel2.visibleKeys = visible;
    content.querySelectorAll("tbody tr[data-key]").forEach((tr) => {
      tr.classList.toggle(
        FILTER_HIDE_CLASS2,
        !!visible && !visible.has(tr.dataset.key)
      );
    });
    virtualizeOnFilterChange();
  }
  function issueFilterNavigation(href) {
    const content = document.getElementById("resource-list-content");
    const input = document.getElementById("ro-filter-input");
    const htmx2 = getHtmx3();
    if (!content || !input || !htmx2) {
      window.location.assign(href);
      return;
    }
    const u = new URL(href, window.location.href);
    const partial = `${u.pathname.replace(/\/+$/, "")}/_table${u.search}`;
    const request = htmx2.ajax("GET", partial, {
      source: input,
      target: "#resource-list-content",
      swap: "morph"
    });
    void request?.catch(() => {
    });
  }
  function commitFilterChip(draft) {
    const text = trimFilterWhitespace(draft);
    const parsed = splitFilterDraft(text);
    if (!parsed) {
      return;
    }
    if (!filterFieldKnown(roRowModel2.fields, parsed.field)) {
      showFilterFieldHint();
      return;
    }
    const raw = encodeURIComponent(text).replace(/%2C/gi, ",");
    const search = window.location.search;
    const href = `${window.location.pathname + (search ? `${search}&` : "?")}f=${raw}`;
    clearFilterDraft();
    issueFilterNavigation(href);
  }
  function popLastFilterChip() {
    const removers = document.querySelectorAll("#ro-filter-field .ro-scope-chip .chip-x");
    if (removers.length === 0) {
      return;
    }
    const href = removers[removers.length - 1].getAttribute("href");
    if (href) {
      issueFilterNavigation(href);
    }
  }
  function clearFilterDraft() {
    const input = document.getElementById("ro-filter-input");
    if (input) {
      input.value = "";
    }
    closeFilterAC();
    applyLiveNameFilter();
  }
  function showFilterFieldHint() {
    const el = document.getElementById("ro-filter-error");
    if (!el) {
      return;
    }
    const names = filterSuggestionFields(roRowModel2.fields).slice(0, 3).map((f) => f.text);
    el.textContent = `no such field — try ${names.join(", ")}…`;
    el.hidden = false;
  }
  function hideFilterFieldHint() {
    const el = document.getElementById("ro-filter-error");
    if (el) {
      el.hidden = true;
    }
  }
  var filterACItems = [];
  var filterACActive = 0;
  function filterACOpen() {
    const ac = document.getElementById("ro-filter-ac");
    return !!ac && !ac.hidden;
  }
  function closeFilterAC() {
    const ac = document.getElementById("ro-filter-ac");
    if (ac) {
      ac.hidden = true;
      ac.textContent = "";
    }
    filterACItems = [];
    filterACActive = 0;
  }
  function openFilterAC(items) {
    const ac = document.getElementById("ro-filter-ac");
    if (!ac || items.length === 0) {
      closeFilterAC();
      return;
    }
    ac.textContent = "";
    ac.setAttribute("role", "listbox");
    filterACItems = items;
    filterACActive = 0;
    items.forEach((item, idx) => {
      const row = document.createElement("div");
      row.className = `ro-ac-item${idx === 0 ? " active" : ""}`;
      row.dataset.roAction = "pick-suggestion";
      row.setAttribute("role", "option");
      row.setAttribute("aria-selected", idx === 0 ? "true" : "false");
      row.dataset.acIndex = String(idx);
      const name = document.createElement("span");
      name.className = "ac-name";
      name.textContent = item.label;
      row.appendChild(name);
      const hint = document.createElement("span");
      hint.className = "ac-hint";
      hint.textContent = item.hint;
      row.appendChild(hint);
      row.addEventListener("mousemove", () => setFilterACActive(idx));
      ac.appendChild(row);
    });
    ac.hidden = false;
  }
  function setFilterACActive(index) {
    filterACActive = Math.max(0, Math.min(filterACItems.length - 1, index));
    const ac = document.getElementById("ro-filter-ac");
    if (!ac) {
      return;
    }
    ac.querySelectorAll('[data-ro-action="pick-suggestion"]').forEach((el) => {
      const on = Number(el.dataset.acIndex) === filterACActive;
      el.classList.toggle("active", on);
      el.setAttribute("aria-selected", on ? "true" : "false");
    });
  }
  function moveFilterACActive(delta) {
    setFilterACActive((filterACActive + delta + filterACItems.length) % filterACItems.length);
  }
  function updateFilterAC() {
    const input = document.getElementById("ro-filter-input");
    if (!input) {
      return;
    }
    const draft = input.value;
    if (!trimFilterWhitespace(draft)) {
      closeFilterAC();
      return;
    }
    const parsed = splitFilterDraft(draft);
    if (!parsed) {
      openFilterAC(rankFieldSuggestions(roRowModel2.fields, draft));
      return;
    }
    if (parsed.op !== ":" || !filterFieldKnown(roRowModel2.fields, parsed.field)) {
      closeFilterAC();
      return;
    }
    openFilterAC(rankValueSuggestions(roRowModel2.fields, roRowModel2.rows, parsed));
  }
  function acceptFilterAC(commitValues) {
    const input = document.getElementById("ro-filter-input");
    const item = filterACItems[filterACActive];
    if (!input || !item) {
      return;
    }
    input.value = item.insert;
    if (item.kind === "value" && commitValues) {
      commitFilterChip(input.value);
    } else {
      applyLiveNameFilter();
      updateFilterAC();
    }
  }
  function handleFilterInputKeydown(event) {
    const input = event.target;
    if (event.key === "Enter") {
      event.preventDefault();
      if (filterACOpen() && filterACItems.length > 0) {
        acceptFilterAC(true);
        return;
      }
      commitFilterChip(input.value);
      return;
    }
    if (event.key === "Tab" && filterACOpen()) {
      event.preventDefault();
      acceptFilterAC(false);
      return;
    }
    if (event.key === "Escape" && filterACOpen()) {
      event.preventDefault();
      closeFilterAC();
      return;
    }
    if (event.key === "ArrowDown" && filterACOpen()) {
      event.preventDefault();
      moveFilterACActive(1);
      return;
    }
    if (event.key === "ArrowUp" && filterACOpen()) {
      event.preventDefault();
      moveFilterACActive(-1);
      return;
    }
    if (event.key === "Backspace" && input.value === "") {
      event.preventDefault();
      popLastFilterChip();
    }
  }
  var filtersBindings = [
    // Chips editor: a chip's ✕ is a real link (no-JS fallback) whose href is
    // the server-built removal URL; intercept it to ride the v2 partial loop
    // (morph + canonical push) instead of a full navigation.
    {
      event: "click",
      selector: '#ro-filter-field [data-ro-action="remove-chip"]',
      handler: (event, matched) => {
        event.preventDefault();
        const href = matched.getAttribute("href");
        if (href) {
          issueFilterNavigation(href);
        }
        return true;
      },
      stop: true
    },
    // Autocomplete row: clicking accepts it (a complete value commits the chip, a
    // field fills `field:` and opens the value suggestions).
    {
      event: "click",
      selector: '#ro-filter-ac [data-ro-action="pick-suggestion"]',
      handler: (event, matched) => {
        event.preventDefault();
        setFilterACActive(Number(matched.dataset.acIndex) || 0);
        acceptFilterAC(true);
        const input = document.getElementById("ro-filter-input");
        if (input) {
          input.focus();
        }
        return true;
      },
      stop: true
    },
    // Clicking the editor field anywhere (the padding, a chip's text) lands the
    // caret in the input -- the whole field reads as one input.
    {
      event: "click",
      selector: "#ro-filter-field",
      handler: (event, matched) => {
        const input = document.getElementById("ro-filter-input");
        if (input && event.target !== input) {
          input.focus();
        }
        void matched;
        return true;
      },
      stop: true
    },
    // C5: a click anywhere outside the editor dismisses the dropdown
    // (esc-equivalent). Independent of the others (listener-inventory C5). No
    // selector (it keys off the closest() escape).
    {
      event: "click",
      handler: (event) => {
        if (!event.target.closest("#ro-filter-field")) {
          closeFilterAC();
        }
      }
    },
    // Chips editor: every keystroke re-runs the live name match (model-
    // driven, NO request) and the autocomplete; a fresh draft clears any
    // unknown-field hint.
    {
      event: "input",
      selector: "#ro-filter-input",
      handler: () => {
        hideFilterFieldHint();
        applyLiveNameFilter();
        updateFilterAC();
        return true;
      },
      stop: true
    },
    // The editor keydown protocol (the focus-routed half of compound case 4):
    // #ro-filter-input owns ⏎ commit/accept, Tab accept, esc dismiss, arrows, and
    // ⌫-on-empty pop. No selector -- it keys off the focused target id, exactly
    // like the still-resident monolith keydown listener it replaces.
    {
      event: "keydown",
      handler: (event) => {
        if (event.target.id === "ro-filter-input") {
          handleFilterInputKeydown(event);
        }
      }
    }
  ];

  // internal/assets/src/js/morph.ts
  var idiomorph = typeof Idiomorph !== "undefined" && typeof Idiomorph.morph === "function" ? Idiomorph : void 0;
  if (idiomorph?.defaults?.callbacks && !window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    const PRIOR = /* @__PURE__ */ new WeakMap();
    idiomorph.defaults.callbacks.beforeNodeMorphed = (oldNode) => {
      if (oldNode.nodeName !== "TD") {
        return;
      }
      PRIOR.set(oldNode, oldNode.textContent);
    };
    idiomorph.defaults.callbacks.afterNodeMorphed = (oldNode) => {
      if (!PRIOR.has(oldNode)) {
        return;
      }
      const el = oldNode;
      const before = PRIOR.get(oldNode);
      PRIOR.delete(oldNode);
      if (before !== el.textContent) {
        el.classList.remove("ro-cell-changed");
        void el.offsetWidth;
        el.classList.add("ro-cell-changed");
      }
    };
  }
  if (typeof htmx !== "undefined" && typeof htmx.defineExtension === "function" && idiomorph) {
    htmx.defineExtension("ro-morph", {
      isInlineSwap: (swapStyle) => swapStyle === "morph",
      handleSwap: (swapStyle, target, fragment) => {
        if (swapStyle !== "morph") {
          return false;
        }
        if (target.id === "resource-list-content") {
          captureRowModel(fragment);
          virtualizePrepareSwap(fragment);
        }
        return idiomorph.morph(target, fragment.children, {
          morphStyle: "innerHTML",
          ignoreActiveValue: true
        });
      }
    });
  }

  // internal/assets/src/js/bulk-actions.ts
  var bulkCopyResetTimer = 0;
  function bulkCopyNames(button) {
    const entries = roRowState2().selectedEntries();
    const names = entries.map((entry) => entry.name).join("\n");
    roCopyText(names, (ok) => {
      if (!ok) {
        return;
      }
      const label = button.querySelector("span:last-child");
      if (!label) {
        return;
      }
      window.clearTimeout(bulkCopyResetTimer);
      label.textContent = "Copied";
      bulkCopyResetTimer = window.setTimeout(() => {
        label.textContent = "Copy names";
      }, 1100);
    });
  }
  function bulkDownloadYAML(bar) {
    if (!bar?.dataset.bulkHref) {
      return;
    }
    const entries = roRowState2().selectedEntries();
    if (entries.length === 0 || entries.length > BULK_NAMES_MAX) {
      return;
    }
    const cluster = bar.dataset.bulkCluster;
    const names = entries.map((entry) => {
      if (bar.dataset.bulkAllns === "true" && cluster && entry.key.startsWith(`${cluster}/`)) {
        return entry.key.slice(cluster.length + 1);
      }
      return entry.name;
    });
    window.location.assign(`${bar.dataset.bulkHref}&names=${encodeURIComponent(names.join(","))}`);
  }
  function roRowState2() {
    return window.roRowState;
  }
  var bulkBindings = [
    {
      event: "click",
      selector: "#ro-bulk-download",
      stop: true,
      handler: (_event, matched) => {
        bulkDownloadYAML(matched.closest("#ro-bulkbar"));
        return true;
      }
    },
    {
      event: "click",
      selector: "#ro-bulk-copy",
      stop: true,
      handler: (_event, matched) => {
        bulkCopyNames(matched);
        return true;
      }
    },
    {
      event: "click",
      selector: "#ro-bulk-clear",
      stop: true,
      handler: () => {
        clearRowState();
        return true;
      }
    }
  ];

  // internal/assets/src/js/columns.ts
  function getHtmx4() {
    return window.htmx;
  }
  var colsPopOpenFlag = false;
  function colsPopOpen() {
    return colsPopOpenFlag;
  }
  function setColsPopOpen(open) {
    colsPopOpenFlag = open;
    const pop = document.getElementById("ro-cols-pop");
    if (pop) {
      pop.classList.toggle("is-open", open);
    }
    const btn = document.getElementById("ro-cols-btn");
    if (btn) {
      btn.setAttribute("aria-expanded", open ? "true" : "false");
    }
  }
  function syncColsPopState() {
    const pop = document.getElementById("ro-cols-pop");
    colsPopOpenFlag = !!pop && pop.classList.contains("is-open");
  }
  function commitColumnVisibility(pop) {
    if (!pop) {
      return;
    }
    const plural = pop.dataset.plural || "";
    if (!plural) {
      return;
    }
    const hidden = [];
    pop.querySelectorAll('[data-ro-action="toggle-column"]').forEach((toggle) => {
      const check = toggle.querySelector(".ro-check");
      if (!toggle.disabled && check && !check.checked && toggle.dataset.col) {
        hidden.push(toggle.dataset.col);
      }
    });
    roPrefsSetHiddenColumns(plural, hidden);
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx4();
    if (content && htmx2) {
      htmx2.trigger(content, "htmx:abort");
    }
    requestListRefresh();
  }
  function popFormMergedHref(form) {
    const owned = /* @__PURE__ */ new Set();
    const fields = [];
    Array.prototype.slice.call(form.elements).forEach((el) => {
      if (el.tagName !== "INPUT" || el.type === "hidden" || !el.name) {
        return;
      }
      owned.add(el.name);
      if (el.value) {
        fields.push(`${el.name}=${encodeURIComponent(el.value)}`);
      }
    });
    return mergeColParams(window.location.pathname, window.location.search, owned, fields);
  }
  var columnsBindings = [
    // Column-visibility popover: the ⊞ title-row button toggles the popover
    // open/closed. Open state is derived from the DOM (a boosted body swap
    // renders it closed). NOT stop:true -- C4's own [data-ro-cols-toggle] guard
    // (the outside-click binding below) keeps the double-fire single, not a stop
    // signal (listener-inventory C1/C4: both see the same click, no propagation
    // stop between them).
    {
      event: "click",
      selector: "[data-ro-cols-toggle]",
      handler: (event) => {
        event.preventDefault();
        const pop = document.getElementById("ro-cols-pop");
        setColsPopOpen(!!pop && !pop.classList.contains("is-open"));
      }
    },
    // A column checkbox row: flip the checkbox optimistically, then commit the
    // COMPLETE hidden set (as the user now sees it) to the ro_prefs cookie and
    // re-render through the container's own programmatic path -- cookie-state,
    // not URL-state: RO-No-Push, zero history entries. The identity row
    // is a disabled <button>, so its clicks never fire.
    {
      event: "click",
      selector: '[data-ro-action="toggle-column"]',
      handler: (event, matched) => {
        event.preventDefault();
        const toggle = matched;
        const check = toggle.querySelector(".ro-check");
        if (check) {
          check.checked = !check.checked;
        }
        commitColumnVisibility(toggle.closest(".ro-pop"));
        return true;
      },
      stop: true
    },
    // C4: a click outside the popover (and not on its ⊞ opener) closes it -- the
    // same dismissal contract the autocomplete dropdown uses. The
    // [data-ro-cols-toggle] escape: when the ⊞ toggle is clicked WHILE open, the
    // toggle binding above already set colsPopOpen=false (closed), and this
    // guard makes this binding a no-op so it does NOT re-toggle (no double-fire /
    // no reopen). No selector (it keys off the flag + the closest() escapes).
    {
      event: "click",
      handler: (event) => {
        if (!colsPopOpenFlag) {
          return;
        }
        const t = event.target;
        if (t.closest("#ro-cols-pop") || t.closest("[data-ro-cols-toggle]")) {
          return;
        }
        setColsPopOpen(false);
      }
    },
    // form.ro-pop-form (the popover's labelcols/selector form): intercept and
    // MERGE into the live query, riding the v2 loop exactly like a chip commit
    // (issueFilterNavigation falls back to a plain navigation when the loop is
    // unavailable). The native submit would rebuild the query from the round-trip
    // hidden inputs alone and wipe every `?f=` chip.
    {
      event: "submit",
      selector: "form.ro-pop-form",
      handler: (event, matched) => {
        event.preventDefault();
        const popForm = matched;
        issueFilterNavigation(popFormMergedHref(popForm));
        return true;
      },
      stop: true
    }
  ];

  // internal/assets/src/js/context-menu.ts
  var CTX_CLAMP_W = 220;
  var CTX_CLAMP_H = 240;
  function closeRowMenu() {
    const menu = document.getElementById("ro-ctxmenu");
    if (menu) {
      menu.classList.remove("is-open");
      menu.setAttribute("aria-hidden", "true");
    }
  }
  function openRowMenu(tr, x, y) {
    const menu = document.getElementById("ro-ctxmenu");
    if (!menu) {
      return;
    }
    const bind = (action, href) => {
      const item = menu.querySelector(`[data-ro-action="${action}"]`);
      if (!item) {
        return;
      }
      if (href) {
        item.dataset.href = href;
        item.hidden = false;
      } else {
        delete item.dataset.href;
        item.hidden = true;
      }
    };
    bind("open", tr.dataset.href);
    bind("yaml", tr.dataset.yaml);
    bind("logs", tr.dataset.logs);
    bind("download", tr.dataset.download);
    const key = tr.dataset.key;
    const name = tr.dataset.name || (key ? lastKeySegment(key) : void 0);
    if (name) {
      menu.dataset.name = name;
    } else {
      delete menu.dataset.name;
    }
    menu.style.left = `${Math.max(8, Math.min(x, window.innerWidth - CTX_CLAMP_W))}px`;
    menu.style.top = `${Math.max(8, Math.min(y, window.innerHeight - CTX_CLAMP_H))}px`;
    menu.classList.add("is-open");
    menu.setAttribute("aria-hidden", "false");
  }
  var contextMenuBindings = [
    // Right-click on an identity row opens the menu; anywhere else closes ours
    // and yields to the native menu.
    {
      event: "contextmenu",
      handler: (event) => {
        const target = event.target;
        const tr = target ? target.closest("#resource-list-content tr[data-key]") : null;
        if (!tr) {
          closeRowMenu();
          return;
        }
        event.preventDefault();
        const me = event;
        openRowMenu(tr, me.clientX, me.clientY);
      }
    },
    // C2 step 1: a context-menu item -> act, then close. Copy stays on the page;
    // the navigation items go through location.assign with the bound data-href.
    // Download YAML is a Content-Disposition attachment, so assigning it
    // downloads WITHOUT leaving the page. Returned in the monolith -> stop:true.
    {
      event: "click",
      selector: "#ro-ctxmenu [data-ro-action]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const item = matched;
        const menu = item.closest("#ro-ctxmenu");
        closeRowMenu();
        if (item.dataset.roAction === "copy") {
          const name = menu?.dataset.name;
          if (name !== void 0) {
            roCopyText(name, () => {
            });
          }
        } else {
          const href = item.dataset.href;
          if (href) {
            window.location.assign(href);
          }
        }
        return true;
      }
    },
    // C2 step 2: ANY other click dismisses an open menu. UNCONDITIONAL and
    // NON-stopping -- the click then FALLS THROUGH to the bulk + row-select
    // bindings (bulk-actions.ts / row-selection.ts), so a click that lands on a
    // row both dismisses the menu AND toggles selection (compound case 1). No
    // selector (it runs on every click, like the monolith's step 2); closeRowMenu
    // on a closed menu is a no-op. NO stop: a stop here would silently drop the
    // selection while still passing a "menu closed" check.
    {
      event: "click",
      handler: () => {
        closeRowMenu();
      }
    },
    // K2: Esc closes the context menu. Its own keydown branch (NO preventDefault),
    // idempotent (closeRowMenu on a closed menu is a no-op).
    {
      event: "keydown",
      handler: (event) => {
        if (event.key === "Escape") {
          closeRowMenu();
        }
      }
    }
  ];

  // internal/assets/src/js/keyboard.ts
  var PALETTE_ID = "ro-palette";
  function roRowState3() {
    return window.roRowState;
  }
  function keyboardTargetIsTextEntry(target) {
    if (!(target instanceof HTMLElement)) {
      return false;
    }
    return target.matches("input, textarea, select") || target.isContentEditable;
  }
  function keyboardSurfaceBusy() {
    const palette = document.getElementById(PALETTE_ID);
    if (palette?.classList.contains("open")) {
      return true;
    }
    const menu = document.getElementById("ro-ctxmenu");
    if (menu?.classList.contains("is-open")) {
      return true;
    }
    const nsDropdown = document.getElementById("namespace-dropdown");
    if (nsDropdown?.classList.contains("is-active")) {
      return true;
    }
    return colsPopOpen();
  }
  function visibleKeyRows() {
    return Array.from(
      document.querySelectorAll("#resource-list-content tbody tr[data-key]")
    ).filter((tr) => !tr.classList.contains("ro-row-filtered"));
  }
  function moveRowFocus(delta) {
    if (virtualizerActive()) {
      return virtMoveFocus(delta);
    }
    const rows = visibleKeyRows();
    if (rows.length === 0) {
      return false;
    }
    const focusKey = roRowState3().focusedKey();
    const current = rows.findIndex((tr) => tr.dataset.key === focusKey);
    const next = Math.max(0, Math.min(rows.length - 1, current + delta));
    roRowState3().setFocus(rows[next].dataset.key);
    rows[next].scrollIntoView({ block: "nearest" });
    return true;
  }
  function openFocusedRow() {
    const key = roRowState3().focusedKey();
    if (!key) {
      return false;
    }
    let row = visibleKeyRows().find((tr) => tr.dataset.key === key) ?? null;
    if (!row && virtualizerActive()) {
      const tr = virtRowByKey(key);
      if (tr && virtVisible().includes(tr)) {
        row = tr;
      }
    }
    if (!row?.dataset.href) {
      return false;
    }
    window.location.assign(row.dataset.href);
    return true;
  }
  var kbdPriorFocus = null;
  function kbdOverlayEl() {
    return document.getElementById("ro-kbd-overlay");
  }
  function kbdOverlayOpen() {
    const overlay = kbdOverlayEl();
    return !!overlay && overlay.classList.contains("open");
  }
  function openKbdOverlay() {
    const overlay = kbdOverlayEl();
    if (!overlay) {
      return;
    }
    kbdPriorFocus = document.activeElement;
    overlay.classList.add("open");
    overlay.setAttribute("aria-hidden", "false");
    const card = overlay.querySelector(".kbd-card");
    if (card) {
      card.focus();
    }
  }
  function closeKbdOverlay() {
    const overlay = kbdOverlayEl();
    if (!overlay) {
      return;
    }
    overlay.classList.remove("open");
    overlay.setAttribute("aria-hidden", "true");
    const prior = kbdPriorFocus;
    if (prior && document.contains(prior)) {
      prior.focus();
    }
    kbdPriorFocus = null;
  }
  var keyboardBindings = [
    // C3: a click on the overlay backdrop ITSELF (outside the card) closes it --
    // the palette's backdrop contract. Independent.
    {
      event: "click",
      handler: (event) => {
        if (event.target === kbdOverlayEl()) {
          closeKbdOverlay();
        }
      }
    },
    // K3: THE gesture keydown. The DOM guards (kbd overlay open, modifier chord,
    // text-entry, surface-busy) keep it disjoint from the palette/filter keys --
    // registration after the palette keydown is incidental; the busy guard does
    // the real work (compound case 2). No selector (it keys off focus/state).
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        if (kbdOverlayOpen()) {
          if (e.key === "Escape" || e.key === "?") {
            e.preventDefault();
            closeKbdOverlay();
          } else if (e.key === "Tab") {
            e.preventDefault();
          }
          return;
        }
        if (e.metaKey || e.ctrlKey || e.altKey) {
          return;
        }
        if (keyboardTargetIsTextEntry(e.target) || keyboardSurfaceBusy()) {
          return;
        }
        if (e.key === "?") {
          e.preventDefault();
          openKbdOverlay();
          return;
        }
        if (e.key === "j" || e.key === "k") {
          if (moveRowFocus(e.key === "j" ? 1 : -1)) {
            e.preventDefault();
          }
          return;
        }
        if (e.key === "Enter") {
          const target = e.target;
          if (target instanceof Element && target.closest("a, button, summary")) {
            return;
          }
          if (openFocusedRow()) {
            e.preventDefault();
          }
        }
      }
    }
  ];

  // internal/assets/src/js/logs.ts
  function logsScrollToTail() {
    const pre = document.querySelector("pre.ro-logpre");
    if (pre) {
      pre.scrollTop = pre.scrollHeight;
    }
  }
  function logsPinTailIfFollowing() {
    const follow = document.getElementById("logFollow");
    if (follow && !follow.classList.contains("quiet")) {
      logsScrollToTail();
    }
  }
  function initLogsFollow() {
    logsPinTailIfFollowing();
  }
  var logsBindings = [
    // Logs Follow toggle: the active accent "Following" sticks the stream
    // to its tail; clicking flips to the quiet "Follow" (and back). Re-activating
    // snaps the stream to the tail immediately. Pure class + label flips -- no
    // request, the read-only floor is untouched. Kept its monolith early-return
    // (stop:true).
    {
      event: "click",
      selector: "#logFollow",
      stop: true,
      handler: (_event, matched) => {
        const logFollow = matched;
        const following = !logFollow.classList.toggle("quiet");
        logFollow.setAttribute("aria-pressed", following ? "true" : "false");
        const label = logFollow.querySelector(".follow-label");
        if (label) {
          label.textContent = following ? "Following" : "Follow";
        }
        if (following) {
          logsScrollToTail();
        }
        return true;
      }
    },
    // Logs display toggles: CLIENT-SIDE only, no refetch. The timestamps
    // checkbox shows/hides the .log-ts spans via the stream's `hide-ts` class.
    // Both flips reflow the stream, so while Following is active the tail is
    // re-pinned afterwards. The monolith #logTs branch early-returned (stop:true).
    {
      event: "change",
      selector: "#logTs",
      stop: true,
      handler: (_event, matched) => {
        const logTs = matched;
        const pre = document.querySelector("pre.ro-logpre");
        if (pre) {
          pre.classList.toggle("hide-ts", !logTs.checked);
          logsPinTailIfFollowing();
        }
        return true;
      }
    },
    // The wrap checkbox toggles `wrap` (pre-wrap + break-word). In the monolith
    // this was the LAST change branch (no branch follows it), so stop:true is the
    // faithful mirror.
    {
      event: "change",
      selector: "#logWrap",
      stop: true,
      handler: (_event, matched) => {
        const logWrap = matched;
        const pre = document.querySelector("pre.ro-logpre");
        if (pre) {
          pre.classList.toggle("wrap", logWrap.checked);
          logsPinTailIfFollowing();
        }
        return true;
      }
    }
  ];

  // internal/assets/src/js/collapse-hash.ts
  function parseCollapsedNames(hash) {
    const names = [];
    hash.replace(/^#/, "").split(";").forEach((param) => {
      const keyVal = param.split("=");
      if (keyVal[0] === "collapsed" && keyVal[1]) {
        keyVal[1].split(",").forEach((name) => {
          if (name) {
            names.push(name);
          }
        });
      }
    });
    return names;
  }

  // internal/assets/src/js/yaml-folds.ts
  function yamlEffectiveIndent(text) {
    const stripped = text.replace(/^\n+/, "");
    const indent = stripped.length - stripped.replace(/^ +/, "").length;
    const rest = stripped.slice(indent);
    if (rest === "-" || rest.startsWith("- ") || rest.startsWith("-	")) {
      return indent + 2;
    }
    return indent;
  }
  function planYamlFolds(indents, isBlank) {
    const bodyCounts = new Array(indents.length).fill(0);
    const ownersByLine = Array.from({ length: indents.length }, () => []);
    const active = [];
    let previous;
    indents.forEach((indent, index) => {
      if (isBlank[index]) {
        return;
      }
      const firstClosed = active.findIndex((owner) => owner.indent >= indent);
      if (firstClosed !== -1) {
        active.splice(firstClosed);
      }
      if (previous && previous.indent < indent) {
        active.push(previous);
      }
      active.forEach((owner) => {
        ownersByLine[index].push(owner.index);
        bodyCounts[owner.index] += 1;
      });
      previous = { index, indent };
    });
    return { bodyCounts, ownersByLine };
  }
  function yamlCodeText(codeCell) {
    const controlSelector = '[data-ro-action="toggle-fold"], [data-ro-fold-control]';
    if (!codeCell.querySelector(controlSelector)) {
      return codeCell.textContent;
    }
    const clone = codeCell.cloneNode(true);
    clone.querySelectorAll(controlSelector).forEach((el) => {
      el.remove();
    });
    return clone.textContent;
  }
  function toggleYamlFold(toggle) {
    const id = toggle.dataset.fold;
    if (!id) {
      return;
    }
    const pre = toggle.closest("pre");
    if (!pre) {
      return;
    }
    const folded = !toggle.classList.contains("is-folded");
    toggle.classList.toggle("is-folded", folded);
    toggle.setAttribute("aria-expanded", folded ? "false" : "true");
    pre.querySelectorAll("[data-fold-of]").forEach((line) => {
      const owners = line.dataset.foldOf.split(" ");
      if (owners.indexOf(id) !== -1) {
        line.classList.toggle("ro-line-folded", folded);
      }
    });
  }
  function injectFoldControls(lineSpan, bodyCount) {
    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.className = "ro-fold-toggle";
    toggle.dataset.roAction = "toggle-fold";
    toggle.setAttribute("aria-expanded", "true");
    toggle.setAttribute("aria-label", "Toggle block");
    toggle.dataset.fold = lineSpan.id;
    const note = document.createElement("span");
    note.className = "ro-fold-note";
    note.dataset.roFoldControl = "note";
    const lineWord = bodyCount === 1 ? "line" : "lines";
    note.textContent = ` … ${bodyCount} ${lineWord}`;
    const anchor = lineSpan.querySelector("a");
    if (anchor?.nextSibling) {
      lineSpan.insertBefore(toggle, anchor.nextSibling);
    } else if (anchor) {
      lineSpan.appendChild(toggle);
    } else {
      lineSpan.insertBefore(toggle, lineSpan.firstChild);
    }
    const last = lineSpan.lastChild;
    if (last?.nodeType === Node.TEXT_NODE && last.data.includes("\n")) {
      lineSpan.insertBefore(note, last);
    } else {
      lineSpan.appendChild(note);
    }
  }
  function buildYamlFolds() {
    document.querySelectorAll(".highlighttable td.code pre").forEach((pre) => {
      if (pre.dataset.roFolds) {
        return;
      }
      try {
        const lines = Array.prototype.filter.call(
          pre.children,
          (el) => el.tagName === "SPAN" && el.id && el.id.indexOf("line-") !== -1
        );
        pre.dataset.roFolds = "1";
        if (lines.length < 3) {
          return;
        }
        const texts = lines.map((line) => line.textContent);
        const indents = texts.map(yamlEffectiveIndent);
        const isBlank = texts.map((text) => text.trim() === "");
        const { bodyCounts, ownersByLine } = planYamlFolds(indents, isBlank);
        lines.forEach((line, index) => {
          const owners = ownersByLine[index];
          if (owners.length === 0) {
            return;
          }
          const element = line;
          const ownerIds = owners.map((owner) => lines[owner].id).join(" ");
          element.dataset.foldOf = ownerIds;
        });
        lines.forEach((line, index) => {
          if (bodyCounts[index] > 0) {
            injectFoldControls(line, bodyCounts[index]);
          }
        });
      } catch (_e) {
      }
    });
  }
  function highlightYamlLine() {
    const fragment = location.hash;
    if (!fragment) {
      return;
    }
    document.querySelectorAll("pre > span.yaml-line-highlight").forEach((el) => {
      el.classList.remove("yaml-line-highlight");
    });
    const element = document.getElementById(`yaml-${fragment.substring(1)}`);
    if (element) {
      element.classList.add("yaml-line-highlight");
      element.scrollIntoView({ block: "center" });
    }
  }
  var foldBindings = [
    // data-ro-action="toggle-fold" (NESTED YAML block fold): toggle the deeper-indented child
    // lines of a `key:`/`- key:` block in place. Matched BEFORE the section-fold
    // + gutter-anchor handlers (registration order) so a nested-fold click never
    // collapses the whole section or jumps a line anchor. The monolith called
    // preventDefault + stopPropagation + return; we keep stopPropagation (inert
    // for document siblings per the inventory, but preserved 1:1) and stop:true
    // mirrors the early return.
    {
      event: "click",
      selector: '[data-ro-action="toggle-fold"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        event.stopPropagation();
        toggleYamlFold(matched);
        return true;
      }
    },
    // YAML line-number anchors (.linenos a): set the URL hash to the clicked
    // line, re-highlight, and suppress the default anchor jump. In the monolith
    // this branch sits AFTER the section-fold branch; here it shares the leaf
    // list and the section-fold handler (misc-ui) is registered separately. The
    // two never co-match (an anchor in the gutter is not a section title), so
    // relative order is immaterial -- but it keeps its own early-return.
    {
      event: "click",
      selector: ".linenos a",
      stop: true,
      handler: (event, matched) => {
        const anchor = matched;
        location.hash = `#${anchor.href.split("#")[1]}`;
        highlightYamlLine();
        event.preventDefault();
        return true;
      }
    }
  ];

  // internal/assets/src/js/misc-ui.ts
  function collapseSectionsFromHash() {
    parseCollapsedNames(document.location.hash).forEach((name) => {
      document.querySelectorAll(`main .collapsible[data-name="${CSS.escape(name)}"]`).forEach((el) => {
        el.classList.add("is-collapsed");
      });
    });
  }
  var miscBindings = [
    // Mobile hamburger: a delegated click on [data-ro-action="toggle-sidebar"] reveals/hides the
    // sidebar by toggling `.is-active` on `.ro-sidebar`. No-op when no sidebar is
    // present (e.g. the Clusters entry page). Kept its early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="toggle-sidebar"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const sidebar = document.querySelector(".ro-sidebar");
        if (sidebar) {
          sidebar.classList.toggle("is-active");
        }
        return true;
      }
    },
    // data-ro-action="copy" (per-section YAML copy): copy THIS section's raw YAML to the
    // clipboard via navigator.clipboard.writeText -- CSP-clean. The raw text is
    // read from the section's Pygments `td.code` cell (the gutter lives in a
    // separate `td.linenos`), with any injected fold controls stripped first
    // (yamlCodeText) so the copy is the full source YAML in any fold state. The
    // button briefly flips its label to "copied". Matched (and stop:true) BEFORE
    // the section-fold binding so a copy click never toggles the section fold.
    {
      event: "click",
      selector: '[data-ro-action="copy"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const copyBtn = matched;
        const section = copyBtn.closest(".collapsible");
        const codeCell = section?.querySelector(".highlighttable td.code");
        const text = codeCell ? yamlCodeText(codeCell) : "";
        const label = copyBtn.querySelector(".ro-copy-text");
        const done = (ok) => {
          if (!label) {
            return;
          }
          label.textContent = ok ? "copied" : "press ⌘C";
          window.setTimeout(() => {
            label.textContent = "copy";
          }, 1500);
        };
        if (navigator.clipboard?.writeText && text) {
          navigator.clipboard.writeText(text).then(
            () => done(true),
            () => done(false)
          );
        } else {
          done(false);
        }
        return true;
      }
    },
    // .collapsible h4.title: toggle `is-collapsed` on the section and sync the
    // URL fragment (collapsed=<names>) with all currently-collapsed sections. The
    // section is resolved via closest('.collapsible') (NOT parentElement) so a
    // Unit-10 YAML card (h4.title nested in .ro-card-head) folds the right node.
    // Registered AFTER the copy binding (copy's stop:true short-circuits a copy
    // click), reproducing the monolith order. Kept its early-return (stop:true).
    {
      event: "click",
      selector: "main .collapsible h4.title",
      stop: true,
      handler: (_event, matched) => {
        const section = matched.closest(".collapsible");
        if (!section) {
          return true;
        }
        section.classList.toggle("is-collapsed");
        const names = [];
        document.querySelectorAll("main .is-collapsed").forEach((el) => {
          const name = el.dataset.name;
          if (name !== void 0) {
            names.push(name);
          }
        });
        if (names.length) {
          document.location.hash = `collapsed=${names.join(",")}`;
        } else {
          window.history.replaceState(
            null,
            document.title,
            window.location.pathname + window.location.search
          );
        }
        return true;
      }
    },
    // Namespace switch: picking a namespace in the topbar dropdown records it
    // as this cluster's last-used namespace in the ro_prefs cookie (server-read
    // only, for cluster-entry hrefs -- never a redirect). The click is
    // deliberately NOT prevented; the boosted navigation proceeds. The cookie
    // write rides the prefs.ts surface directly (the same seam legacy uses).
    // Kept its early-return (stop:true).
    {
      event: "click",
      selector: '#namespace-dropdown [data-ro-action="pick-namespace"]',
      stop: true,
      handler: (_event, matched) => {
        const href = matched.getAttribute("href");
        const hrefMatch = href ? /^\/clusters\/([^/]+)\/namespaces\/([^/]+)\//.exec(href) : null;
        if (hrefMatch) {
          roPrefsSetNamespace(
            decodeURIComponent(hrefMatch[1]),
            decodeURIComponent(hrefMatch[2])
          );
        }
        return true;
      }
    },
    // #namespace-dropdown .context-trigger: toggle `is-active`; focus the
    // searchbox when opening. Kept its early-return (stop:true).
    {
      event: "click",
      selector: "#namespace-dropdown .context-trigger",
      stop: true,
      handler: (_event, matched) => {
        const nsDropdown = matched.closest("#namespace-dropdown");
        if (!nsDropdown) {
          return true;
        }
        nsDropdown.classList.toggle("is-active");
        if (nsDropdown.classList.contains("is-active")) {
          const searchbox = document.getElementById("namespace-searchbox");
          if (searchbox) {
            searchbox.focus();
          }
        }
        return true;
      }
    },
    // #namespace-searchbox input: filter the [data-ro-action="pick-namespace"] links by
    // case-insensitive substring. Terminal branch in the monolith input listener
    // (no branch followed it), reproduced as stop:true.
    {
      event: "input",
      selector: "#namespace-searchbox",
      stop: true,
      handler: (_event, matched) => {
        const filterText = matched.value.toLowerCase();
        document.querySelectorAll('[data-ro-action="pick-namespace"]').forEach((element) => {
          const text = element.innerText.toLowerCase();
          if (text.indexOf(filterText) === -1) {
            element.classList.add("is-hidden");
          } else {
            element.classList.remove("is-hidden");
          }
        });
        return true;
      }
    },
    // #namespace-searchbox keyup: Enter selects the first still-visible match.
    // Sole branch of the monolith keyup listener; stop:true mirrors its return.
    {
      event: "keyup",
      selector: "#namespace-searchbox",
      stop: true,
      handler: (event) => {
        if (event.key !== "Enter") {
          return true;
        }
        const firstVisible = Array.from(
          document.querySelectorAll('[data-ro-action="pick-namespace"]')
        ).find((element) => !element.classList.contains("is-hidden"));
        firstVisible?.click();
        return true;
      }
    },
    // In-cell +N overflow (label/selector chips and data keys): the `.ro-chip.more[data-ro-more]` button
    // toggles `.expanded` on its OWN `.ro-chips` strip, revealing the `.xtra` chips
    // in place (the button face flips +N <-> "less" in CSS). Delegated so it
    // survives every morph; aria-expanded mirrors the state. A refresh morph
    // re-renders the strip collapsed (server truth) -- expansion is a transient
    // peek, not persisted state. Was a trailing branch of the monolith big click
    // listener (C1); kept its early-return (stop:true).
    {
      event: "click",
      selector: "[data-ro-more]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const chips = matched.closest(".ro-chips");
        if (chips) {
          const expanded = chips.classList.toggle("expanded");
          matched.setAttribute("aria-expanded", expanded ? "true" : "false");
        }
        return true;
      }
    },
    // Long-annotation toggle: a >120-char annotation renders as a
    // collapsed `key · size` button + a hidden scrollable <pre> payload. The
    // delegated click flips the [hidden] attribute on the sibling .anno-pre,
    // mirrors the state into aria-expanded, and rotates the chevron via the .open
    // class -- CSP-clean and morph-safe (server truth re-renders collapsed; a
    // transient peek, like the chip overflow above). C1 trailing branch;
    // early-return (stop:true).
    {
      event: "click",
      selector: "[data-ro-annolong]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const annoToggle = matched;
        const pre = annoToggle.parentElement ? annoToggle.parentElement.querySelector(".anno-pre") : null;
        if (pre) {
          const open = pre.hidden !== false;
          pre.hidden = !open;
          annoToggle.setAttribute("aria-expanded", open ? "true" : "false");
          annoToggle.classList.toggle("open", open);
        }
        return true;
      }
    },
    // data-ro-action="toggle-tools": toggle `is-active` on the control itself and on the element
    // named by its `data-target`. C1 trailing branch; early-return (stop:true).
    {
      event: "click",
      selector: '[data-ro-action="toggle-tools"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        const toggle = matched;
        toggle.classList.toggle("is-active");
        const targetEl = toggle.dataset.target ? document.getElementById(toggle.dataset.target) : null;
        if (targetEl) {
          targetEl.classList.toggle("is-active");
        }
        return true;
      }
    },
    // Search-button enable (change): a checkbox carries `data-ro-toggle-button="<id>"`.
    // The named button is enabled iff any checkbox sharing that same value is
    // checked, else disabled. Was the lead branch of the monolith change listener;
    // early-return (stop:true).
    {
      event: "change",
      selector: "input[data-ro-toggle-button]",
      stop: true,
      handler: (_event, matched) => {
        const buttonId = matched.dataset.roToggleButton;
        const button = buttonId ? document.getElementById(buttonId) : null;
        if (button) {
          const anyChecked = document.querySelectorAll(`input[data-ro-toggle-button="${buttonId}"]:checked`).length > 0;
          button.disabled = !anyChecked;
        }
        return true;
      }
    },
    // form.tools-form (the v1 multi-type tools form): on submit, blank the `name`
    // of empty inputs so they do not become empty query parameters in the
    // resulting GET URL. Sole branch of the monolith submit listener; it did NOT
    // early-return (the form still submits), so this binding does NOT stop.
    {
      event: "submit",
      selector: "form.tools-form",
      handler: (_event, matched) => {
        const form = matched;
        Array.prototype.slice.call(form.getElementsByTagName("input")).forEach((input) => {
          if (input.name && !input.value) {
            input.name = "";
          }
        });
      }
    }
  ];

  // internal/assets/src/js/palette-rank.ts
  var WORD_SEPARATORS = " -_./:";
  function isAsciiUppercase(character) {
    return character >= "A" && character <= "Z";
  }
  function roFuzzyScore(query, text) {
    const source = text;
    const q = query.toLowerCase();
    const t = source.toLowerCase();
    if (!q) {
      return 0;
    }
    const first = t.indexOf(q[0]);
    if (first === -1) {
      return -1;
    }
    let from = first + 1;
    for (let i = 1; i < q.length; i++) {
      const at = t.indexOf(q[i], from);
      if (at === -1) {
        return -1;
      }
      from = at + 1;
    }
    const gaps = from - first - q.length;
    let tier = 2;
    if (gaps === 0) {
      if (first === 0) {
        tier = 0;
      } else {
        const separatorBoundary = WORD_SEPARATORS.includes(t[first - 1]);
        const camelHump = isAsciiUppercase(source[first]) && !isAsciiUppercase(source[first - 1]);
        if (separatorBoundary || camelHump) {
          tier = 1;
        }
      }
    }
    return tier * 1e5 + gaps * 100 + Math.min(first, 99);
  }
  function rankPaletteEntries(list, query, labelOf) {
    if (!query) {
      return list.slice();
    }
    const scored = [];
    list.forEach((entry) => {
      const score = roFuzzyScore(query, labelOf(entry));
      if (score >= 0) {
        scored.push({ entry, score });
      }
    });
    scored.sort((a, b) => a.score - b.score);
    return scored.map((it) => it.entry);
  }
  function paletteRecentTarget(entry) {
    return entry.href ? `href:${entry.href}` : `action:${entry.action}`;
  }
  function dedupeRecents(prior, entry, max) {
    const kept = prior.filter((it) => paletteRecentTarget(it) !== paletteRecentTarget(entry));
    kept.unshift(entry);
    return kept.slice(0, max);
  }
  var FEED_GROUPS = [
    { title: "Resource types", key: "kinds" },
    { title: "Namespaces", key: "namespaces" },
    { title: "Clusters", key: "clusters" },
    { title: "Actions", key: "actions" }
  ];
  function feedEntryLabel(entry, key) {
    if (key === "kinds") {
      return String(entry.kind || entry.plural || "");
    }
    return String(entry.name || entry.label || "");
  }
  function buildPaletteGroups(query, feed, recents, pageObjects) {
    const q = query.trim();
    const groups = [];
    if (q) {
      groups.push({ title: "Everywhere", key: "everywhere", entries: [{ query: q }] });
    } else if (recents.length > 0) {
      groups.push({ title: "Recents", key: "recents", entries: recents.slice() });
    }
    const objects = rankPaletteEntries(pageObjects, q, (o) => o.name);
    if (objects.length > 0) {
      groups.push({ title: "On this page", key: "objects", entries: objects });
    }
    FEED_GROUPS.forEach((group) => {
      const list = feed[group.key] || [];
      const ranked = rankPaletteEntries(list, q, (entry) => feedEntryLabel(entry, group.key));
      if (ranked.length > 0) {
        groups.push({ title: group.title, key: group.key, entries: ranked });
      }
    });
    return groups;
  }

  // internal/assets/src/js/palette.ts
  var PALETTE_ID2 = "ro-palette";
  window.roFuzzy = roFuzzyScore;
  function asPropertyRecord(value) {
    return Object(value);
  }
  function nonEmptyString(value) {
    if (typeof value !== "string") {
      return void 0;
    }
    const trimmed = value.trim();
    return trimmed.length > 0 ? trimmed : void 0;
  }
  function feedEntryLabel2(entry, kindEntry) {
    return kindEntry ? nonEmptyString(entry.kind) ?? nonEmptyString(entry.plural) : nonEmptyString(entry.name) ?? nonEmptyString(entry.label);
  }
  function readPaletteData() {
    const empty = {
      currentCluster: null,
      currentNamespace: null,
      clusters: [],
      namespaces: [],
      kinds: [],
      actions: []
    };
    const el = document.getElementById("ro-palette-data");
    if (!el) {
      return empty;
    }
    try {
      const data = asPropertyRecord(JSON.parse(el.textContent));
      const records = (value, kindEntries = false) => Array.isArray(value) ? value.map(asPropertyRecord).filter((entry) => feedEntryLabel2(entry, kindEntries) !== void 0).filter(
        (entry) => paletteHrefSafe(entry.href) !== "" || nonEmptyString(entry.action) !== void 0
      ) : [];
      return {
        currentCluster: nonEmptyString(data.currentCluster) ?? null,
        currentNamespace: nonEmptyString(data.currentNamespace) ?? null,
        clusters: records(data.clusters),
        namespaces: records(data.namespaces),
        kinds: records(data.kinds, true),
        actions: records(data.actions)
      };
    } catch {
      return empty;
    }
  }
  function paletteHrefSafe(href, pageHref = window.location.href) {
    if (typeof href !== "string") {
      return "";
    }
    const trimmed = href.trim();
    try {
      const page = new URL(pageHref);
      const parsed = new URL(trimmed, page);
      const networkProtocol = parsed.protocol === "http:" || parsed.protocol === "https:";
      return networkProtocol && parsed.origin === page.origin ? trimmed : "";
    } catch {
      return "";
    }
  }
  var PALETTE_RECENTS_KEY = "ro-pref-recents";
  var PALETTE_RECENTS_MAX = 5;
  function readPaletteRecents() {
    try {
      const raw = window.localStorage.getItem(PALETTE_RECENTS_KEY);
      if (!raw) {
        return [];
      }
      const list = JSON.parse(raw);
      const recents = [];
      const seen = /* @__PURE__ */ new Set();
      for (const value of list) {
        const record = asPropertyRecord(value);
        const label = nonEmptyString(record.label);
        if (!label) {
          continue;
        }
        const href = paletteHrefSafe(record.href);
        const action = nonEmptyString(record.action);
        if (!href && !action) {
          continue;
        }
        const recent = { label, href: href || void 0, action };
        const target = paletteRecentTarget(recent);
        if (seen.has(target)) {
          continue;
        }
        seen.add(target);
        recents.push(recent);
        if (recents.length === PALETTE_RECENTS_MAX) {
          break;
        }
      }
      return recents;
    } catch {
      return [];
    }
  }
  function recordPaletteRecent(label, href, action) {
    if (!label || !href && !action) {
      return;
    }
    const entry = { label, href, action };
    const kept = dedupeRecents(readPaletteRecents(), entry, PALETTE_RECENTS_MAX);
    try {
      window.localStorage.setItem(PALETTE_RECENTS_KEY, JSON.stringify(kept));
    } catch {
    }
  }
  var paletteRows = [];
  var paletteActive = 0;
  var paletteScope = {
    cluster: null,
    namespace: null
  };
  function createPaletteRow() {
    const row = document.createElement("div");
    row.className = "ro-pal-item";
    row.dataset.roAction = "pick-palette-row";
    row.setAttribute("role", "option");
    return row;
  }
  function appendPaletteLabel(row, text) {
    const label = document.createElement("span");
    label.className = "pal-label";
    label.textContent = text;
    row.appendChild(label);
    return label;
  }
  function buildPaletteRow(entry, key) {
    const row = createPaletteRow();
    const kindRow = key === "kinds";
    const icon = nonEmptyString(entry.icon);
    if (kindRow && icon) {
      const holder = document.createElement("template");
      holder.innerHTML = icon;
      row.appendChild(holder.content);
    }
    const labelText = feedEntryLabel2(entry, kindRow);
    const display = nonEmptyString(entry.display) ?? labelText;
    const label = appendPaletteLabel(row, display);
    if (display !== labelText) {
      row.title = labelText;
    }
    const entryName = nonEmptyString(entry.name);
    const currentScope = key === "clusters" ? paletteScope.cluster : key === "namespaces" ? paletteScope.namespace : null;
    if (entryName && entryName === currentScope) {
      const ctx = document.createElement("span");
      ctx.className = "pal-ctx";
      ctx.textContent = "current";
      label.appendChild(ctx);
    }
    if (kindRow) {
      const meta = document.createElement("span");
      meta.className = "pal-meta";
      meta.textContent = nonEmptyString(entry.group) ?? "core";
      row.appendChild(meta);
      const scope = document.createElement("span");
      const namespaced = entry.namespaced === true;
      scope.className = `pal-scope ${namespaced ? "ns" : "cluster"}`;
      scope.textContent = namespaced ? "namespaced" : "cluster";
      row.appendChild(scope);
    }
    const href = paletteHrefSafe(entry.href);
    if (href) {
      row.dataset.href = href;
    }
    const action = nonEmptyString(entry.action);
    if (action) {
      row.dataset.action = action;
    }
    row.dataset.label = labelText;
    return row;
  }
  function buildEverywhereRow(query) {
    const row = createPaletteRow();
    const glyph = document.querySelector(`#${PALETTE_ID2} .ro-pal-search .ico`);
    if (glyph) {
      row.appendChild(glyph.cloneNode(true));
    }
    const labelText = `Search all clusters for “${query}”`;
    appendPaletteLabel(row, labelText);
    row.dataset.href = `/search?q=${encodeURIComponent(query)}`;
    row.dataset.label = labelText;
    return row;
  }
  var PALETTE_STATUS_TONES = ["ok", "warn", "err", "info", "mute"];
  function harvestPageObjects() {
    const out = [];
    const rows = virtualizerActive() ? virtRows() : document.querySelectorAll("#resource-list-content table.ro-table tbody tr");
    Array.prototype.forEach.call(rows, (tr) => {
      const a = tr.querySelector("td.cell-name a");
      if (!a) {
        return;
      }
      const href = paletteHrefSafe(a.getAttribute("href"));
      const name = a.textContent.trim();
      if (!href || !name) {
        return;
      }
      const object = { name, href };
      const st = tr.querySelector(".cell-status");
      if (st) {
        object.status = st.textContent.trim();
        object.tone = PALETTE_STATUS_TONES.find(
          (candidate) => st.classList.contains(candidate)
        );
      }
      out.push(object);
    });
    return out;
  }
  function buildObjectRow(o) {
    const row = createPaletteRow();
    appendPaletteLabel(row, o.name);
    if (o.status) {
      const st = document.createElement("span");
      st.className = `pal-status${o.tone ? ` ${o.tone}` : ""}`;
      st.textContent = o.status;
      row.appendChild(st);
    }
    row.dataset.href = o.href;
    row.dataset.label = o.name;
    return row;
  }
  function renderPalette(query) {
    const list = document.getElementById("ro-palette-list");
    if (!list) {
      return;
    }
    const data = readPaletteData();
    paletteScope.cluster = data.currentCluster;
    paletteScope.namespace = data.currentNamespace;
    const scope = document.getElementById("ro-palette-scope");
    if (scope) {
      const scopeText = paletteScope.namespace ?? paletteScope.cluster;
      scope.textContent = scopeText ?? "";
      scope.hidden = scopeText === null;
    }
    const q = query;
    list.replaceChildren();
    paletteRows = [];
    const rowFor = (item, key) => {
      switch (key) {
        case "everywhere":
          return buildEverywhereRow(item.query);
        case "objects":
          return buildObjectRow(item);
        default:
          return buildPaletteRow(item, key);
      }
    };
    const groups = buildPaletteGroups(
      q,
      {
        clusters: data.clusters,
        namespaces: data.namespaces,
        kinds: data.kinds,
        actions: data.actions
      },
      readPaletteRecents(),
      harvestPageObjects()
    );
    groups.forEach((group) => {
      const heading = document.createElement("div");
      heading.className = "ro-pal-group";
      heading.textContent = group.title;
      list.appendChild(heading);
      group.entries.forEach((item) => {
        const row = rowFor(item, group.key);
        const idx = paletteRows.length;
        row.addEventListener("mousemove", () => setPaletteActive(idx));
        list.appendChild(row);
        paletteRows.push({ el: row });
      });
    });
    if (paletteRows.length === 0) {
      const none = document.createElement("div");
      none.className = "ro-pal-empty";
      none.textContent = "No matching targets.";
      list.appendChild(none);
    }
    paletteActive = 0;
    paintPaletteActive();
  }
  function paintPaletteActive() {
    paletteRows.forEach((r, i) => {
      const on = i === paletteActive;
      r.el.classList.toggle("active", on);
      r.el.setAttribute("aria-selected", on ? "true" : "false");
    });
    if (paletteRows[paletteActive]) {
      paletteRows[paletteActive].el.scrollIntoView({ block: "nearest" });
    }
  }
  function setPaletteActive(index) {
    paletteActive = index;
    paintPaletteActive();
  }
  function movePaletteActive(delta) {
    const length = paletteRows.length;
    paletteActive = (paletteActive + delta + length) % length;
    paintPaletteActive();
  }
  function choosePaletteRow(rowEl) {
    const action = rowEl.dataset.action;
    const href = rowEl.dataset.href;
    recordPaletteRecent(rowEl.dataset.label, href, action);
    closePalette();
    if (action === "theme") {
      document.getElementById("btn-theme-toggle")?.click();
      return;
    }
    if (href) {
      window.location.assign(href);
    }
  }
  function activatePaletteSelection() {
    const active = paletteRows[paletteActive];
    if (active) {
      choosePaletteRow(active.el);
    }
  }
  var palettePriorFocus = null;
  var paletteRestoringFocus;
  function openPalette(prefill) {
    const palette = document.getElementById(PALETTE_ID2);
    const input = document.getElementById("ro-palette-input");
    if (!palette || !input) {
      return;
    }
    if (!palette.classList.contains("open")) {
      palettePriorFocus = document.activeElement;
    }
    palette.classList.add("open");
    palette.setAttribute("aria-hidden", "false");
    input.value = typeof prefill === "string" ? prefill : "";
    renderPalette(input.value);
    input.focus();
  }
  window.roOpenPalette = openPalette;
  function closePalette() {
    const palette = document.getElementById(PALETTE_ID2);
    if (!palette) {
      return;
    }
    palette.classList.remove("open");
    palette.setAttribute("aria-hidden", "true");
    const prior = palettePriorFocus;
    palettePriorFocus = null;
    if (!prior || !document.contains(prior) || palette.contains(prior)) {
      return;
    }
    const focus = prior.focus;
    if (typeof focus !== "function") {
      return;
    }
    paletteRestoringFocus = true;
    try {
      focus.call(prior);
    } finally {
      paletteRestoringFocus = void 0;
    }
  }
  var paletteBindings = [
    // ⌘K palette result row: a click on a result row activates it (navigate or
    // run its named action, then close). FIRST so a click inside the open
    // palette never falls through to a page handler. (C1 head, returned.)
    {
      event: "click",
      selector: '[data-ro-action="pick-palette-row"]',
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        choosePaletteRow(matched);
        return true;
      }
    },
    // The read-only topbar search box ([data-ro-palette-open]) opens the palette on
    // click instead of typing inline. (C1, returned.)
    {
      event: "click",
      selector: "[data-ro-palette-open]",
      stop: true,
      handler: (event) => {
        event.preventDefault();
        openPalette();
        return true;
      }
    },
    // The search page's "Refine · ⌘K" button: open the palette PREFILLED
    // with the query the page searched (server-baked data-query). (C1, returned.)
    {
      event: "click",
      selector: "[data-ro-search-refine]",
      stop: true,
      handler: (event, matched) => {
        event.preventDefault();
        openPalette(matched.dataset.query);
        return true;
      }
    },
    // A click on the palette backdrop ITSELF (the dimmed area outside the panel)
    // closes it, like Esc. A click inside the panel does not match. The selector
    // is the backdrop root id; the handler still verifies that the original
    // target is that matched root, so a descendant click does NOT close it.
    {
      event: "click",
      selector: `#${PALETTE_ID2}`,
      stop: true,
      handler: (event, matched) => {
        if (event.target === matched) {
          closePalette();
          return true;
        }
        return false;
      }
    },
    // ⌘K palette query box: re-render the grouped rows fuzzy-matched + ranked
    // against the label, re-seating the active row. (Monolith input head, returned.)
    {
      event: "input",
      selector: "#ro-palette-input",
      stop: true,
      handler: (_event, matched) => {
        renderPalette(matched.value);
        return true;
      }
    },
    // ⌘K / Ctrl+K chord opens the palette from anywhere (ignored with Alt/Shift,
    // so an unrelated OS/browser shortcut is never hijacked). The palette is
    // exclusive: an open "?" overlay or row menu closes FIRST so one Esc later
    // closes exactly one surface. No selector (it keys off the chord, not a
    // delegated target). Does NOT stop: the still-resident gesture keydown (K3)
    // returns on the modifier chord on its own, and the filter editor's keydown
    // is unaffected -- mirroring the monolith's separate listeners.
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === "k" || e.key === "K")) {
          e.preventDefault();
          closeKbdOverlay();
          closeRowMenu();
          openPalette();
        }
      }
    },
    // Palette-open keyboard model (Esc/Arrow/Enter/Tab). Acts ONLY while the
    // palette is open AND the target is not the filter editor: in the monolith
    // the filter-input keydown branch RETURNED before this palette branch, so an
    // Escape with focus in #ro-filter-input routed to the filter handler and
    // never reached closePalette (compound case 4). The still-resident filter
    // keydown listener keeps owning #ro-filter-input keys; this binding excludes
    // that target so the focus-routed Escape semantics are byte-identical. No
    // stop: the gesture keydown (K3) is kept inert by keyboardSurfaceBusy()
    // (palette `.open`), the real decoupler.
    {
      event: "keydown",
      handler: (event) => {
        const e = event;
        const target = e.target;
        if (target && target.id === "ro-filter-input") {
          return;
        }
        const palette = document.getElementById(PALETTE_ID2);
        if (!palette?.classList.contains("open")) {
          return;
        }
        if (e.key === "Escape") {
          e.preventDefault();
          closePalette();
        } else if (e.key === "ArrowDown") {
          e.preventDefault();
          movePaletteActive(1);
        } else if (e.key === "ArrowUp") {
          e.preventDefault();
          movePaletteActive(-1);
        } else if (e.key === "Enter") {
          e.preventDefault();
          activatePaletteSelection();
        } else if (e.key === "Tab") {
          e.preventDefault();
          movePaletteActive(e.shiftKey ? -1 : 1);
        }
      }
    },
    // The topbar search box also opens the palette on keyboard FOCUS (Tab-into /
    // programmatic focus): focusin bubbles to document. openPalette runs FIRST
    // (while the box still holds focus) so it captures the box as the Esc restore
    // target; the blur after only matters when openPalette no-opped. The
    // paletteRestoringFocus gate keeps the close-restore from re-opening: focusing
    // the box FROM closePalette fires this very binding.
    {
      event: "focusin",
      selector: "[data-ro-palette-open]",
      handler: (event) => {
        if (paletteRestoringFocus) {
          return;
        }
        openPalette();
        const t = event.target;
        t.blur();
      }
    }
  ];

  // internal/assets/src/js/bindings.ts
  var bindings = [
    ...contextMenuBindings,
    ...bulkBindings,
    ...rowSelectionBindings,
    ...columnsBindings,
    ...filtersBindings,
    ...paletteBindings,
    ...keyboardBindings,
    ...foldBindings,
    ...logsBindings,
    // misc-ui's click bindings keep their relative monolith order: copy is
    // registered before the section-fold binding (copy stop:true short-circuits
    // a copy click), so a copy click never folds its section. misc-ui now also
    // carries the trailing presentation toggles ([data-ro-more] / [data-ro-annolong] /
    // [data-ro-action="toggle-tools"]) and the v1 form glue (the data-ro-toggle-button
    // change + the tools-form submit) lifted out of the dismantled legacy.js.
    ...miscBindings,
    // refresh-domain tails LAST: the retry + set-refresh hooks were
    // the monolith big click listener's own trailing branches, so registering
    // them after the migrated leaves preserves the C1 order -- every leaf
    // front-ran the monolith, and these ran at its end. Neither co-matches any
    // selector above, so the position is observationally free; LAST documents
    // their monolith origin.
    ...refreshBindings
  ];

  // internal/assets/src/js/events.ts
  function closestElement(event, selector) {
    const target = event.target;
    const element = target instanceof Element ? target : target?.parentElement;
    return element?.closest(selector) ?? null;
  }
  function dispatch(bindings2, event) {
    for (const binding of bindings2) {
      let matched = null;
      if (binding.selector !== void 0) {
        matched = closestElement(event, binding.selector);
        if (!matched) {
          continue;
        }
      }
      let result;
      try {
        result = binding.handler(event, matched);
      } catch (e) {
        console.warn("readout event binding failed", binding.event, binding.selector, e);
        continue;
      }
      if (binding.stop && result) {
        return;
      }
    }
  }
  function registerBindings(bindings2) {
    const byType = /* @__PURE__ */ new Map();
    for (const binding of bindings2) {
      const list = byType.get(binding.event);
      if (list) {
        list.push(binding);
      } else {
        byType.set(binding.event, [binding]);
      }
    }
    byType.forEach((list, type) => {
      document.addEventListener(type, (event) => dispatch(list, event));
    });
  }

  // internal/assets/src/js/register-bindings.ts
  registerBindings(bindings);

  // internal/assets/src/js/theme.ts
  var PREFERS_DARK = window.matchMedia("(prefers-color-scheme: dark)");
  function syncThemeTogglePostTarget() {
    const toggle = document.getElementById("btn-theme-toggle");
    if (!toggle) {
      return;
    }
    if (toggle.dataset.themeExplicit !== "false") {
      return;
    }
    const form = toggle.form;
    const input = form?.querySelector('input[name="theme"]');
    if (input) {
      input.value = PREFERS_DARK.matches ? "light" : "dark";
    }
  }
  PREFERS_DARK.addEventListener("change", syncThemeTogglePostTarget);

  // internal/assets/src/js/toasts.ts
  var TOAST_VISIBLE_MS = 3500;
  var TOAST_LEAVE_MS = 200;
  function showToast(message) {
    const host = document.getElementById("ro-toasts");
    if (!host) {
      return;
    }
    const toast = document.createElement("div");
    toast.className = "ro-toast";
    toast.textContent = message;
    host.appendChild(toast);
    window.setTimeout(() => {
      toast.classList.add("is-leaving");
      window.setTimeout(() => toast.remove(), TOAST_LEAVE_MS);
    }, TOAST_VISIBLE_MS);
  }

  // internal/assets/src/js/skeleton.ts
  function listRegionIsEmpty(content) {
    return content.childElementCount === 0;
  }
  document.addEventListener("htmx:beforeRequest", (event) => {
    if (!isListRefreshEvent(event)) {
      return;
    }
    const content = document.getElementById("resource-list-content");
    const template = document.getElementById("ro-skel-template");
    if (!content || !template || !listRegionIsEmpty(content)) {
      return;
    }
    content.replaceChildren(...Array.from(template.children, (node) => node.cloneNode(true)));
  });
  function clearListSkeleton() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    content.querySelectorAll(":scope > .ro-skel").forEach((skeleton) => {
      skeleton.remove();
    });
  }
  document.addEventListener("htmx:responseError", (event) => {
    if (isListRefreshEvent(event)) {
      clearListSkeleton();
    }
  });
  document.addEventListener("htmx:sendError", (event) => {
    if (isListRefreshEvent(event)) {
      clearListSkeleton();
    }
  });

  // internal/assets/src/js/init.ts
  window.roToast = showToast;
  function handleSortPreferenceRequest(event) {
    const detail = Object(event.detail);
    const rawCfg = Object(detail.requestConfig);
    const cfg = {
      ...rawCfg,
      headers: Object(rawCfg.headers)
    };
    const target = Object(detail.target);
    if (target.id !== "resource-list-content") {
      return;
    }
    if (cfg.headers["RO-No-Push"] || cfg.headers["HX-Preloaded"] === "true") {
      return;
    }
    const elt = Object(detail.elt);
    let sortHeader = null;
    try {
      sortHeader = elt.closest("thead th");
    } catch {
    }
    if (!sortHeader) {
      return;
    }
    let plural;
    let sort;
    try {
      const requestURL = new URL(String(cfg.path), window.location.href);
      const rawSort = requestURL.searchParams.get("sort");
      if (!rawSort) {
        return;
      }
      const pathMatch = /\/([^/]+)\/_table$/.exec(requestURL.pathname);
      if (!pathMatch) {
        return;
      }
      sort = rawSort;
      plural = decodeURIComponent(pathMatch[1]);
    } catch {
      return;
    }
    roPrefsSetSort(plural, sort);
  }
  document.addEventListener("htmx:beforeRequest", handleSortPreferenceRequest);
  document.addEventListener("htmx:afterSwap", (event) => {
    if (isListRefreshEvent(event)) {
      rememberListValidator(event);
      noteRefreshRecovery();
      clearListStale();
      reapplyRowState();
      applyLiveNameFilter();
      const filterInput = document.getElementById("ro-filter-input");
      if (filterInput && document.activeElement === filterInput && filterInput.value) {
        updateFilterAC();
      }
      if (colsPopOpen()) {
        setColsPopOpen(true);
      }
      virtualizeAfterSwap();
      liveOnListSwap(event);
    }
  });
  document.addEventListener("htmx:beforeSwap", (event) => {
    const detail = event.detail;
    if (suppressListNotModified(event)) {
      noteRefreshRecovery();
      clearListStale();
      return;
    }
    if (detail && detail.target === document.body) {
      const status = detail.xhr?.status;
      if (typeof status === "number" && status >= 400 && status <= 599) {
        detail.shouldSwap = true;
      }
      closeRowMenu();
      clearRowState();
      clearListStale();
      liveTeardown();
      liveState.status = "idle";
      liveState.streamPath = "";
    }
  });
  document.addEventListener("htmx:historyRestore", () => {
    reapplyRowState();
    updateBulkBar();
  });
  function setupStickyNamespace() {
    document.querySelectorAll(".ro-table-wrap table.ro-table").forEach((table) => {
      const firstCell = table.querySelector("tbody tr:not(.ro-vspacer) td:first-child");
      if (firstCell?.classList.contains("cell-ns")) {
        table.style.setProperty(
          "--ns-col-w",
          `${firstCell.getBoundingClientRect().width}px`
        );
        table.classList.add("ro-sticky2");
      } else {
        table.classList.remove("ro-sticky2");
        table.style.removeProperty("--ns-col-w");
      }
    });
  }
  function runInitStep(step) {
    try {
      step();
    } catch (e) {
      console.warn("readout init step failed", e);
    }
  }
  function runInit() {
    [
      syncRefreshUI,
      // Live stream reconciliation, BEFORE applyRefresh so
      // the poll chain arms against fresh live state: a riding stream
      // disarms it (effective 0), a fallback sets the 5s cadence.
      liveApply,
      applyRefresh,
      buildYamlFolds,
      collapseSectionsFromHash,
      highlightYamlLine,
      initLogsFollow,
      syncThemeTogglePostTarget,
      setupStickyNamespace,
      // Chips-editor row model: captured from the full server-rendered
      // document. ORDER CONTRACT: this step must stay BEFORE the windowing
      // init that prunes rows from the DOM -- at this point
      // the DOM still IS the complete dataset.
      captureRowModelFromDocument,
      // Virtualization engagement: windows the >threshold
      // table the server marked `.ro-windowed`. AFTER the model capture,
      // per the order contract above.
      virtualizeInit,
      // Columns-popover open flag: re-derived from the fresh DOM so a
      // boosted body swap (rendered closed) never leaves a stale-open flag.
      syncColsPopState,
      // Row state is keyed by OBJECT identity; the store clears when an
      // hx-boost navigation swaps the body (the htmx:beforeSwap hook above),
      // so this init re-paint scrubs any stale is-selected classes a
      // cached/boosted body carried in -- and the bulk bar re-syncs to the
      // same store right after.
      reapplyRowState,
      updateBulkBar
    ].forEach(runInitStep);
  }
  document.addEventListener("DOMContentLoaded", runInit);
  document.addEventListener("htmx:load", runInit);
  document.addEventListener("htmx:afterSettle", setupStickyNamespace);
  window.addEventListener("resize", setupStickyNamespace);
})();
