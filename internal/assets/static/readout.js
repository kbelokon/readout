"use strict";
(() => {
  // internal/assets/src/js/htmx-config.ts
  if (typeof htmx !== "undefined") {
    htmx.config.globalViewTransitions = false;
  }

  // internal/assets/src/js/filters-parse.ts
  var GO_FIELD_WHITESPACE_TRIM = /^\p{White_Space}+|\p{White_Space}+$/gu;
  var GO_FIELD_WHITESPACE_RUN = /\p{White_Space}+/gu;
  function trimFilterWhitespace(s) {
    return (s || "").replace(GO_FIELD_WHITESPACE_TRIM, "");
  }
  function normalizeFieldWhitespace(s) {
    return trimFilterWhitespace(s).replace(GO_FIELD_WHITESPACE_RUN, " ");
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

  // internal/assets/src/js/list-projection.ts
  var rowModel = {
    fields: [],
    rows: [],
    visibleKeys: null
  };
  function emptySnapshot() {
    return {
      rows: [],
      byKey: /* @__PURE__ */ new Map(),
      indexByKey: /* @__PURE__ */ new Map(),
      order: [],
      cardsByKey: /* @__PURE__ */ new Map(),
      fields: [],
      modelRows: [],
      windowed: false
    };
  }
  var projection = emptySnapshot();
  var prepared = null;
  var projectionRoot = null;
  var projectionRevision = 0;
  window.roRowModel = rowModel;
  function captureFields(table) {
    return Array.from(table.querySelectorAll("thead th")).map((th) => {
      const label = normalizeFieldWhitespace(th.textContent || "");
      return {
        label,
        name: fieldSuggestionText(label),
        hint: th.dataset.hint || ""
      };
    });
  }
  function captureModelRow(tr) {
    const cells = Array.from(tr.querySelectorAll("td")).map(
      (td) => trimFilterWhitespace(td.textContent || "")
    );
    const nameLink = tr.querySelector("td.cell-name a");
    return {
      key: tr.dataset.key,
      name: nameLink ? trimFilterWhitespace(nameLink.textContent || "") : cells[0] || "",
      cells
    };
  }
  function captureModelRows(rows) {
    return rows.map(captureModelRow);
  }
  function captureCards(root) {
    const cards = Array.from(root.querySelectorAll(".ro-cardlist > .ro-pcard"));
    const cardsByKey = /* @__PURE__ */ new Map();
    cards.forEach((card) => {
      const key = card.dataset.key;
      if (key) {
        cardsByKey.set(key, card);
      }
    });
    return cardsByKey;
  }
  function snapshotFrom(root) {
    const table = root.querySelector("table.ro-table");
    if (!table) {
      return emptySnapshot();
    }
    const tbody = table.tBodies.item(0);
    if (!tbody || tbody.querySelector(":scope > tr.ro-vspacer")) {
      return emptySnapshot();
    }
    const rows = Array.from(tbody.querySelectorAll(":scope > tr[data-key]"));
    const byKey = new Map(rows.map((row) => [row.dataset.key, row]));
    const order = rows.map((row) => row.dataset.key);
    return {
      rows,
      byKey,
      indexByKey: new Map(order.map((key, index) => [key, index])),
      order,
      cardsByKey: captureCards(root),
      fields: captureFields(table),
      modelRows: captureModelRows(rows),
      windowed: table.closest(".ro-table-wrap.ro-windowed") !== null
    };
  }
  function publishModel(snapshot) {
    rowModel.fields = snapshot.fields;
    rowModel.rows = snapshot.modelRows;
  }
  function adoptListProjection(root) {
    projection = snapshotFrom(root);
    projectionRoot = root;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
  }
  function ensureListProjection(root) {
    if (projectionRoot === root) {
      return false;
    }
    adoptListProjection(root);
    return true;
  }
  function prepareListProjectionSwap(root) {
    if (prepared?.root !== root) {
      const snapshot = snapshotFrom(root);
      projectionRevision += 1;
      prepared = {
        root,
        snapshot,
        // Snapshot maps are created once and never mutated or exposed. Keep
        // the immutable prior index by reference for the windowed cell diff.
        previousByKey: projection.byKey
      };
      publishModel(snapshot);
    }
    return {
      rows: prepared.snapshot.rows,
      windowed: prepared.snapshot.windowed
    };
  }
  function commitListProjectionSwap() {
    if (!prepared) {
      return null;
    }
    const incoming = prepared;
    prepared = null;
    const content = document.getElementById("resource-list-content");
    if (incoming.snapshot.windowed) {
      projection = incoming.snapshot;
      projectionRoot = content || incoming.root;
    } else {
      projection = content ? snapshotFrom(content) : emptySnapshot();
      projectionRoot = content;
      publishModel(projection);
    }
    return incoming.previousByKey;
  }
  function resetListProjection() {
    projection = emptySnapshot();
    projectionRoot = null;
    prepared = null;
    projectionRevision += 1;
    publishModel(projection);
    rowModel.visibleKeys = null;
  }
  function listProjectionSwapPending() {
    return prepared !== null;
  }
  function listProjectionRevision() {
    return projectionRevision;
  }
  function listProjectionWindowed() {
    return projection.windowed;
  }
  function listProjectionRows() {
    return projection.rows;
  }
  function listProjectionRowByKey(key) {
    return projection.byKey.get(key) || null;
  }
  function listProjectionRowModel() {
    return rowModel;
  }
  function setListProjectionVisibleKeys(keys) {
    rowModel.visibleKeys = keys;
  }
  function listProjectionVisibleRows() {
    const keys = rowModel.visibleKeys;
    return keys ? projection.rows.filter((row) => keys.has(row.dataset.key)) : projection.rows;
  }
  var focusableSelector = "a[href], button, input, select, textarea, [tabindex]";
  function oneElementRoot(parent) {
    let root = null;
    for (const node of parent.childNodes) {
      if (node instanceof Text && !node.data.trim()) continue;
      if (root || !(node instanceof HTMLElement)) return null;
      root = node;
    }
    return root;
  }
  function parseRowFragment(html, key) {
    const tbody = document.createElement("tbody");
    tbody.innerHTML = html;
    const row = oneElementRoot(tbody);
    return row?.dataset.key === key ? row : null;
  }
  function parseCardFragment(html, key) {
    const template = document.createElement("template");
    template.innerHTML = html;
    const card = oneElementRoot(template.content);
    return card?.dataset.key === key ? card : null;
  }
  function parseRegionFragment(content, update) {
    const selector = `[data-ro-live-region="${update.region}"]`;
    const current = content.querySelector(selector);
    const template = document.createElement("template");
    template.innerHTML = update.html;
    const incoming = oneElementRoot(template.content);
    if (!current || !incoming || incoming.dataset.roLiveRegion !== update.region) return null;
    return { current, incoming };
  }
  function currentListMounts() {
    const content = document.getElementById("resource-list-content");
    if (!content || projectionRoot !== content || prepared) return null;
    const table = content.querySelector("table.ro-table");
    const tbody = table?.tBodies.item(0) || null;
    if (!tbody) return null;
    return {
      cardMount: content.querySelector(".ro-cardlist"),
      content,
      tbody
    };
  }
  function arraysEqual(left, right) {
    return left.length === right.length && left.every((value, index) => value === right[index]);
  }
  function morphElement(current, incoming) {
    const implementation = Idiomorph;
    const outcome = implementation.morph(current, incoming, {
      morphStyle: "outerHTML",
      ignoreActiveValue: true
    });
    if (Array.isArray(outcome)) {
      const landed = outcome.find((node) => node instanceof HTMLElement);
      if (landed instanceof HTMLElement) return landed;
    }
    return current;
  }
  function placeElementsInOrder(parent, elements) {
    let cursor = parent.firstElementChild;
    for (const element of elements) {
      if (element === cursor) {
        cursor = cursor.nextElementSibling;
      } else {
        parent.insertBefore(element, cursor);
      }
    }
  }
  function captureFocusBookmark() {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement)) return null;
    const row = active.closest("tr[data-key]");
    const key = row?.dataset.key;
    const cell = active.closest("td, th");
    if (!row || !key || !cell || cell.parentElement !== row) return null;
    const focusables = cellFocusTargets(cell);
    const focusableIndex = focusables.indexOf(active);
    if (focusableIndex === -1) return null;
    return {
      active,
      cellIndex: Array.from(row.cells).indexOf(cell),
      focusableIndex,
      key
    };
  }
  function cellFocusTargets(cell) {
    return [cell, ...cell.querySelectorAll(focusableSelector)];
  }
  function focusRestorer(bookmark) {
    return () => {
      if (!bookmark) return;
      const row = projection.byKey.get(bookmark.key);
      if (!row?.isConnected) return;
      const cell = row.cells.item(bookmark.cellIndex);
      if (!(cell instanceof HTMLElement)) return;
      const focusables = cellFocusTargets(cell);
      focusables[bookmark.focusableIndex]?.focus({ preventScroll: true });
    };
  }
  function updateLiveStatus(summary) {
    const status = document.getElementById("ro-live-status");
    if (!status) return;
    const changed = summary.inserted + summary.updated + summary.deleted + summary.projected;
    const parts = [];
    if (changed > 0) parts.push(`${changed} row${changed === 1 ? "" : "s"}`);
    if (summary.reordered) parts.push("order changed");
    if (summary.regions.length > 0) {
      parts.push(`${summary.regions.length} region${summary.regions.length === 1 ? "" : "s"}`);
    }
    status.textContent = `Live update: ${parts.join(", ")}`;
  }
  function applyListProjectionDelta(plan) {
    const mounts = currentListMounts();
    if (!mounts) return { ok: false };
    const parsedRows = /* @__PURE__ */ new Map();
    const parsedCards = /* @__PURE__ */ new Map();
    const parsedRegions = [];
    for (const operation of plan.upsert) {
      const row = parseRowFragment(operation.row, operation.key);
      if (!row) return { ok: false };
      parsedRows.set(operation.key, row);
      if (operation.card !== void 0) {
        const card = parseCardFragment(operation.card, operation.key);
        if (!card || !mounts.cardMount) return { ok: false };
        parsedCards.set(operation.key, card);
      }
    }
    for (const operation of plan.regions) {
      const region = parseRegionFragment(mounts.content, operation);
      if (!region) return { ok: false };
      parsedRegions.push(region);
    }
    const removedKeys = new Set(plan.remove.map((operation) => operation.key));
    const nextByKey = new Map(projection.byKey);
    const nextCards = new Map(projection.cardsByKey);
    for (const key of removedKeys) {
      nextByKey.delete(key);
    }
    for (const [key, incoming] of parsedRows) {
      nextByKey.set(key, projection.byKey.get(key) || incoming);
    }
    for (const [key, incoming] of parsedCards) {
      nextCards.set(key, projection.cardsByKey.get(key) || incoming);
    }
    const implicitOrder = projection.order.filter((key) => !removedKeys.has(key));
    for (const key of parsedRows.keys()) {
      if (!projection.byKey.has(key)) implicitOrder.push(key);
    }
    const nextOrder = plan.order ? [...plan.order] : implicitOrder;
    if (nextOrder.length !== nextByKey.size || nextOrder.some((key) => !nextByKey.has(key))) {
      return { ok: false };
    }
    const nextRowSet = new Set(nextOrder);
    const focus = captureFocusBookmark();
    const restoreFocus = focusRestorer(focus);
    const previousByKey = /* @__PURE__ */ new Map();
    for (const { key } of plan.upsert) {
      const current = projection.byKey.get(key);
      if (current) previousByKey.set(key, current.cloneNode(true));
    }
    const summary = {
      inserted: plan.upsert.filter((operation) => !projection.byKey.has(operation.key)).length,
      updated: plan.upsert.filter((operation) => projection.byKey.has(operation.key)).length,
      deleted: plan.remove.filter((operation) => operation.cause === "delete").length,
      projected: plan.remove.filter((operation) => operation.cause === "project").length,
      reordered: !arraysEqual(nextOrder, projection.order),
      regions: plan.regions.map((operation) => operation.region)
    };
    try {
      for (const operation of plan.remove) {
        projection.byKey.get(operation.key)?.remove();
        projection.cardsByKey.get(operation.key)?.remove();
      }
      for (const [key, incoming] of parsedRows) {
        const current = projection.byKey.get(key);
        if (current) nextByKey.set(key, morphElement(current, incoming));
      }
      for (const [key, incoming] of parsedCards) {
        const current = projection.cardsByKey.get(key);
        if (current) nextCards.set(key, morphElement(current, incoming));
      }
      for (const region of parsedRegions) morphElement(region.current, region.incoming);
      const nextRows = nextOrder.map((key) => nextByKey.get(key));
      const nextCardEntries = nextOrder.flatMap((key) => {
        const card = nextCards.get(key);
        return card ? [[key, card]] : [];
      });
      if (!projection.windowed) placeElementsInOrder(mounts.tbody, nextRows);
      if (mounts.cardMount) {
        placeElementsInOrder(
          mounts.cardMount,
          nextCardEntries.map(([, card]) => card)
        );
      }
      const byKey = new Map(nextOrder.map((key, index) => [key, nextRows[index]]));
      projection = {
        rows: nextRows,
        byKey,
        indexByKey: new Map(nextOrder.map((key, index) => [key, index])),
        order: nextOrder,
        cardsByKey: new Map(nextCardEntries),
        fields: projection.fields,
        modelRows: captureModelRows(nextRows),
        windowed: projection.windowed
      };
      projectionRoot = mounts.content;
      prepared = null;
      projectionRevision += 1;
      publishModel(projection);
      if (rowModel.visibleKeys) {
        rowModel.visibleKeys = new Set(
          Array.from(rowModel.visibleKeys).filter((key) => nextRowSet.has(key))
        );
      }
      updateLiveStatus(summary);
      return { ok: true, focusKey: focus?.key || null, previousByKey, summary, restoreFocus };
    } catch {
      restoreFocus();
      resetListProjection();
      return { ok: false };
    }
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
    if (!sourceIsContent || detail.target !== void 0 && !targetIsContent || headerValue(headers, "RO-No-Push") !== "true") {
      return;
    }
    if (content.childElementCount === 0) {
      clearContentValidator(content);
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
  var RECONNECT_DELAY_LADDER_MS = [1e3, 2e3, 5e3, 1e4, 3e4];
  function reconnectDelayMs(attempt, random = Math.random) {
    const rung = Number.isInteger(attempt) && attempt >= 1 ? attempt : 1;
    const cap = RECONNECT_DELAY_LADDER_MS[Math.min(rung, RECONNECT_DELAY_LADDER_MS.length) - 1];
    const roll = random();
    const fraction = Number.isFinite(roll) ? Math.min(Math.max(roll, 0), 1) : 1;
    return Math.round(cap * fraction);
  }
  var RETRY_AFTER_MAX_MS = 3e5;
  var RETRY_AFTER_MIN_MS = 1e3;
  function retryAfterMs(header, now = Date.now()) {
    if (header === null) return null;
    const value = header.trim();
    if (value === "") return null;
    if (/^\d+$/.test(value)) {
      return clampRetryAfter(Number(value) * 1e3);
    }
    if (!/^[a-z]{3}/i.test(value)) return null;
    const at = Date.parse(value);
    if (Number.isNaN(at)) return null;
    return clampRetryAfter(at - now);
  }
  function clampRetryAfter(ms) {
    return Math.min(Math.max(ms, RETRY_AFTER_MIN_MS), RETRY_AFTER_MAX_MS);
  }
  var HEALTHY_CONTINUITY_MS = 3e4;
  function shouldResetBackoff(snapshotAt2, now) {
    if (snapshotAt2 <= 0) return false;
    return now - snapshotAt2 >= HEALTHY_CONTINUITY_MS;
  }

  // internal/assets/src/js/live-protocol.ts
  var LIST_DELTA_APPLIED_EVENT = "ro:list-delta-applied";
  var BASE_FIELDS = /* @__PURE__ */ new Set(["v", "kind", "g", "seq", "rev", "rv", "schema"]);
  var decodedEnvelopes = /* @__PURE__ */ new WeakSet();
  function own(record, key) {
    return Object.hasOwn(record, key);
  }
  function exactFields(record, allowed) {
    return Object.keys(record).every((key) => allowed.has(key));
  }
  function nonemptyString(value) {
    return typeof value === "string" && value.length > 0;
  }
  function seal(value) {
    decodedEnvelopes.add(value);
    return value;
  }
  function decodeBase(record) {
    return record.v === 2 && nonemptyString(record.g) && Number.isSafeInteger(record.seq) && record.seq > 0 && (!own(record, "rev") || nonemptyString(record.rev)) && (!own(record, "rv") || nonemptyString(record.rv)) && (!own(record, "schema") || nonemptyString(record.schema));
  }
  function decodeSnapshot(record) {
    const snapshot = record.snapshot;
    if (!exactFields(record, /* @__PURE__ */ new Set([...BASE_FIELDS, "snapshot"])) || !nonemptyString(record.rev) || !nonemptyString(record.schema) || !exactFields(snapshot, /* @__PURE__ */ new Set(["html"])) || !nonemptyString(snapshot.html)) {
      return { ok: false };
    }
    return { ok: true, value: seal(record) };
  }
  function decodeRemove(value) {
    if (!Array.isArray(value)) return null;
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (const valueItem of value) {
      const item = valueItem;
      if (!exactFields(item, /* @__PURE__ */ new Set(["key", "cause"])) || !nonemptyString(item.key) || item.cause !== "delete" && item.cause !== "project" || seen.has(item.key)) {
        return null;
      }
      seen.add(item.key);
      result.push({ key: item.key, cause: item.cause });
    }
    return result;
  }
  function decodeUpsert(value) {
    if (!Array.isArray(value)) return null;
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (const valueItem of value) {
      const item = valueItem;
      if (!exactFields(item, /* @__PURE__ */ new Set(["key", "row", "card"])) || !nonemptyString(item.key) || !nonemptyString(item.row) || own(item, "card") && !nonemptyString(item.card) || seen.has(item.key)) {
        return null;
      }
      seen.add(item.key);
      result.push(
        own(item, "card") ? { key: item.key, row: item.row, card: item.card } : { key: item.key, row: item.row }
      );
    }
    return result;
  }
  function decodeOrder(value) {
    if (!Array.isArray(value)) return null;
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (const key of value) {
      if (!nonemptyString(key) || seen.has(key)) return null;
      seen.add(key);
      result.push(key);
    }
    return result;
  }
  function decodeRegions(value) {
    if (!Array.isArray(value)) return null;
    const seen = /* @__PURE__ */ new Set();
    const result = [];
    for (const valueItem of value) {
      const item = valueItem;
      if (!exactFields(item, /* @__PURE__ */ new Set(["region", "html"])) || item.region !== "count" && item.region !== "phase" && item.region !== "found" || !nonemptyString(item.html) || seen.has(item.region)) {
        return null;
      }
      seen.add(item.region);
      result.push({ region: item.region, html: item.html });
    }
    return result;
  }
  function decodeDelta(record) {
    const wireDelta = record.delta;
    if (!exactFields(record, /* @__PURE__ */ new Set([...BASE_FIELDS, "delta"])) || !nonemptyString(record.rev) || !nonemptyString(record.schema) || !exactFields(wireDelta, /* @__PURE__ */ new Set(["base", "rev", "remove", "upsert", "order", "regions"])) || !nonemptyString(wireDelta.base) || !nonemptyString(wireDelta.rev) || wireDelta.rev !== record.rev) {
      return { ok: false };
    }
    const delta = { base: wireDelta.base, rev: wireDelta.rev };
    if (own(wireDelta, "remove")) {
      const remove = decodeRemove(wireDelta.remove);
      if (!remove) return { ok: false };
      delta.remove = remove;
    }
    if (own(wireDelta, "upsert")) {
      const upsert = decodeUpsert(wireDelta.upsert);
      if (!upsert) return { ok: false };
      delta.upsert = upsert;
    }
    const removed = new Set(delta.remove?.map((operation) => operation.key));
    if (delta.upsert?.some((operation) => removed.has(operation.key))) {
      return { ok: false };
    }
    if (own(wireDelta, "order")) {
      const order = decodeOrder(wireDelta.order);
      if (!order) return { ok: false };
      delta.order = order;
    }
    if (own(wireDelta, "regions")) {
      const regions = decodeRegions(wireDelta.regions);
      if (!regions) return { ok: false };
      delta.regions = regions;
    }
    return {
      ok: true,
      value: seal({ ...record, delta })
    };
  }
  function decodeTerminal(record) {
    if (!exactFields(record, /* @__PURE__ */ new Set([...BASE_FIELDS, "reason"])) || record.reason !== "auth" && record.reason !== "lifetime" && record.reason !== "shutdown" && record.reason !== "watch-failed") {
      return { ok: false };
    }
    return { ok: true, value: seal(record) };
  }
  function decodeLiveV2Envelope(frame) {
    try {
      const parsed = JSON.parse(frame);
      if (!decodeBase(parsed)) {
        return { ok: false };
      }
      if (parsed.kind === "snapshot") return decodeSnapshot(parsed);
      if (parsed.kind === "delta") return decodeDelta(parsed);
      if (parsed.kind === "terminal") return decodeTerminal(parsed);
      return { ok: false };
    } catch {
      return { ok: false };
    }
  }
  function validateCursor(envelope, cursor) {
    return envelope.g === cursor.g && envelope.seq === cursor.seq + 1 && envelope.delta.base === cursor.rev && envelope.schema === cursor.schema;
  }
  function decodeApplyInput(input) {
    if (typeof input === "string") return decodeLiveV2Envelope(input);
    if (typeof input === "object" && input !== null && decodedEnvelopes.has(input)) {
      return { ok: true, value: input };
    }
    return { ok: false };
  }
  function applyLiveV2Delta(input, cursor) {
    const decoded = decodeApplyInput(input);
    if (!decoded.ok || decoded.value.kind !== "delta" || !validateCursor(decoded.value, cursor)) {
      return { ok: false };
    }
    const envelope = decoded.value;
    const plan = {
      remove: envelope.delta.remove || [],
      upsert: envelope.delta.upsert || [],
      order: envelope.delta.order,
      regions: envelope.delta.regions || []
    };
    const applied = applyListProjectionDelta(plan);
    if (!applied.ok) return applied;
    const detail = {
      kind: "delta",
      deletedKeys: new Set(
        plan.remove.filter((operation) => operation.cause === "delete").map((operation) => operation.key)
      ),
      focusKey: applied.focusKey,
      previousByKey: applied.previousByKey,
      summary: applied.summary
    };
    document.dispatchEvent(
      new CustomEvent(LIST_DELTA_APPLIED_EVENT, { detail })
    );
    applied.restoreFocus();
    const nextCursor = {
      g: envelope.g,
      seq: envelope.seq,
      rev: envelope.rev,
      schema: envelope.schema
    };
    if (envelope.rv !== void 0) nextCursor.rv = envelope.rv;
    else if (cursor.rv !== void 0) nextCursor.rv = cursor.rv;
    return { ok: true, cursor: nextCursor, summary: applied.summary };
  }

  // internal/assets/src/js/bounded-byte-buffer.ts
  function ensureBoundedByteBufferCapacity(buffer, retainedBytes, appendedBytes, hardLimit) {
    const required = retainedBytes + appendedBytes;
    if (required <= buffer.byteLength) return buffer;
    const capacity = Math.min(
      hardLimit,
      buffer.byteLength + Math.max(buffer.byteLength, appendedBytes)
    );
    const grown = new Uint8Array(capacity);
    grown.set(buffer.subarray(0, retainedBytes));
    return grown;
  }

  // internal/assets/src/js/live-sse.ts
  var LIVE_SSE_DATA_CEILING_BYTES = 16777216;
  var LIVE_SSE_FRAMED_CEILING_BYTES = 16778240;
  var LIVE_SSE_LIMITS = Object.freeze({
    dataBytes: LIVE_SSE_DATA_CEILING_BYTES,
    // All nonblank field/comment bytes in one uncommitted event. This prevents
    // ignored extension/comment lines from multiplying bounded data work.
    eventBytes: LIVE_SSE_FRAMED_CEILING_BYTES,
    eventNameBytes: 64,
    lines: 32,
    // A legal max-size one-line payload still carries `data:` plus optional
    // whitespace. Keep a small, fixed framing allowance separate from the
    // exact aggregate data ceiling.
    lineBytes: LIVE_SSE_FRAMED_CEILING_BYTES
  });
  var LiveSSEError = class extends Error {
    code;
    constructor(code) {
      super(code);
      this.code = code;
    }
  };
  var fatalUTF8 = new TextDecoder("utf-8", { fatal: true });
  function decodeLine(bytes) {
    try {
      return fatalUTF8.decode(bytes);
    } catch {
      throw new LiveSSEError("invalid-utf8");
    }
  }
  var LiveSSEParser = class {
    #limits;
    #lineBuffer = new Uint8Array();
    #lineBytes = 0;
    #pendingDelimiter = "none";
    #eventName = null;
    #eventBytes = 0;
    #dataLines = [];
    #dataBytes = 0;
    #lines = 0;
    #fatal = null;
    constructor(limits = {}) {
      this.#limits = { ...LIVE_SSE_LIMITS, ...limits };
    }
    push(chunk) {
      if (this.#fatal) throw this.#fatal;
      const events = [];
      try {
        this.#consume(chunk, events);
        return events;
      } catch (error) {
        this.#fatal = error instanceof LiveSSEError ? error : new LiveSSEError("invalid-utf8");
        throw this.#fatal;
      }
    }
    #consume(chunk, events) {
      let start = 0;
      for (let index = 0, byte = chunk[index]; byte !== void 0; index += 1, byte = chunk[index]) {
        if (this.#pendingDelimiter === "cr") {
          this.#pendingDelimiter = "none";
          if (byte === 10) {
            start = index + 1;
            continue;
          }
        }
        if (byte !== 10 && byte !== 13) continue;
        this.#appendLinePart(chunk.subarray(start, index));
        this.#completeLine(events);
        this.#pendingDelimiter = byte === 13 ? "cr" : "none";
        start = index + 1;
      }
      this.#appendLinePart(chunk.subarray(start));
    }
    #appendLinePart(part) {
      if (part.byteLength > this.#limits.lineBytes - this.#lineBytes) {
        throw new LiveSSEError("line-too-large");
      }
      const required = this.#lineBytes + part.byteLength;
      this.#lineBuffer = ensureBoundedByteBufferCapacity(
        this.#lineBuffer,
        this.#lineBytes,
        part.byteLength,
        this.#limits.lineBytes
      );
      this.#lineBuffer.set(part, this.#lineBytes);
      this.#lineBytes = required;
    }
    #completeLine(events) {
      const bytes = this.#lineBuffer.subarray(0, this.#lineBytes);
      this.#lineBytes = 0;
      if (bytes.byteLength === 0) {
        if (this.#dataLines.length > 0) {
          events.push({
            name: this.#eventName,
            data: this.#dataLines.join("\n"),
            dataBytes: this.#dataBytes
          });
        }
        this.#resetEvent();
        return;
      }
      if (bytes.byteLength > this.#limits.eventBytes - this.#eventBytes) {
        throw new LiveSSEError("event-too-large");
      }
      this.#eventBytes += bytes.byteLength;
      this.#lines += 1;
      if (this.#lines > this.#limits.lines) {
        throw new LiveSSEError("too-many-lines");
      }
      const colon = bytes.indexOf(58);
      const fieldEnd = colon === -1 ? bytes.byteLength : colon;
      let valueStart = colon === -1 ? bytes.byteLength : colon + 1;
      if (bytes[valueStart] === 32) valueStart += 1;
      const field = decodeLine(bytes.subarray(0, fieldEnd));
      const valueBytes = bytes.subarray(valueStart);
      if (field === "event") {
        if (valueBytes.byteLength > this.#limits.eventNameBytes) {
          throw new LiveSSEError("event-name-too-large");
        }
        this.#eventName = decodeLine(valueBytes);
        return;
      }
      if (field !== "data") {
        decodeLine(valueBytes);
        return;
      }
      const joinedBytes = this.#dataBytes + valueBytes.byteLength + (this.#dataLines.length ? 1 : 0);
      if (joinedBytes > this.#limits.dataBytes) {
        throw new LiveSSEError("data-too-large");
      }
      this.#dataLines.push(decodeLine(valueBytes));
      this.#dataBytes = joinedBytes;
    }
    #resetEvent() {
      this.#eventName = null;
      this.#eventBytes = 0;
      this.#dataLines = [];
      this.#dataBytes = 0;
      this.#lines = 0;
    }
  };

  // internal/assets/src/js/live-url.ts
  var CLIENT_GENERATION_HEX_LENGTH = 32;
  var CLIENT_GENERATION_UUID_LENGTH = 36;
  var CLIENT_GENERATION_UUID_DASHES = /* @__PURE__ */ new Set([8, 13, 18, 23]);
  var ASCII_HEX_DIGITS = "0123456789abcdefABCDEF";
  function isClientLiveGeneration(value) {
    if (typeof value !== "string" || value.length !== CLIENT_GENERATION_HEX_LENGTH && value.length !== CLIENT_GENERATION_UUID_LENGTH) {
      return false;
    }
    const uuid = value.length === CLIENT_GENERATION_UUID_LENGTH;
    return Array.from(value).every(
      (character, index) => uuid && CLIENT_GENERATION_UUID_DASHES.has(index) ? character === "-" : ASCII_HEX_DIGITS.includes(character)
    );
  }
  function mintLiveGeneration(cryptoSource = window.crypto) {
    try {
      const uuid = cryptoSource.randomUUID();
      if (isClientLiveGeneration(uuid)) return uuid;
    } catch {
    }
    const bytes = cryptoSource.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  }
  function withRawQuery(pathname, rawQuery) {
    return rawQuery === "" ? pathname : `${pathname}?${rawQuery}`;
  }
  function liveStreamBaseForURL(url) {
    if (url.origin !== window.location.origin) return "";
    const pathname = `${url.pathname.replace(/\/+$/, "")}/_stream`;
    if (!pathname.startsWith("/") || pathname.startsWith("//")) return "";
    return withRawQuery(pathname, url.search.slice(1));
  }

  // internal/assets/src/js/stale.ts
  var staleCountdownId = null;
  var staleRetryAt = 0;
  var listStaleOwner = /* @__PURE__ */ Symbol();
  var liveStaleOwner = /* @__PURE__ */ Symbol();
  var liveUnavailableOwner = /* @__PURE__ */ Symbol();
  var staleOwners = /* @__PURE__ */ new Set();
  var liveSemanticStale = false;
  var liveGraceTimerId;
  var LIVE_STALE_GRACE_MS = 3e3;
  function bannerElement() {
    return document.querySelector(".ro-stale-banner");
  }
  function bannerParts(banner) {
    return {
      recoverable: banner.querySelector(".bn-body:not(.ro-stale-unavailable)"),
      reload: banner.querySelector(".ro-stale-reload"),
      retry: banner.querySelector(".ro-stale-retry"),
      unavailable: banner.querySelector(".bn-body.ro-stale-unavailable")
    };
  }
  function paintBannerVariant(banner, unavailable) {
    const parts = bannerParts(banner);
    if (parts.recoverable) parts.recoverable.hidden = unavailable;
    if (parts.unavailable) parts.unavailable.hidden = !unavailable;
    if (parts.retry) parts.retry.hidden = unavailable;
    if (parts.reload) parts.reload.hidden = !unavailable;
  }
  function stopStaleCountdown() {
    if (staleCountdownId !== null) {
      window.clearInterval(staleCountdownId);
      staleCountdownId = null;
    }
  }
  function startStaleCountdown() {
    if (staleCountdownId === null) {
      staleCountdownId = window.setInterval(updateStaleCountdown, 1e3);
    }
    updateStaleCountdown();
  }
  function clearLiveGrace() {
    window.clearTimeout(liveGraceTimerId);
    liveGraceTimerId = void 0;
  }
  function paintStaleState() {
    const listStale = staleOwners.has(listStaleOwner) || staleOwners.has(liveStaleOwner);
    const liveUnavailable = staleOwners.has(liveUnavailableOwner);
    const content = document.getElementById("resource-list-content");
    if (content) {
      content.classList.toggle("ro-stale", listStale || liveUnavailable);
      if (liveSemanticStale) content.dataset.roStale = "true";
      else delete content.dataset.roStale;
    }
    const banner = bannerElement();
    if (!banner) {
      stopStaleCountdown();
      return;
    }
    paintBannerVariant(banner, liveUnavailable);
    banner.hidden = !(listStale || liveUnavailable);
    if (banner.hidden) stopStaleCountdown();
    else startStaleCountdown();
  }
  function noteStaleRetryAt(atMs) {
    staleRetryAt = atMs;
    updateStaleCountdown();
  }
  function updateStaleCountdown() {
    const banner = bannerElement();
    if (!banner) return;
    const span = banner.querySelector("[data-stale-countdown]");
    if (!span) {
      return;
    }
    const nextAt = staleRetryAt;
    if (!nextAt) {
      span.textContent = "…";
      return;
    }
    const remaining = Math.max(0, Math.ceil((nextAt - Date.now()) / 1e3));
    span.textContent = `${remaining}s`;
  }
  function isListRefreshEvent(event) {
    const detail = event.detail;
    if (!detail) {
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
    staleOwners.add(listStaleOwner);
    paintStaleState();
  }
  function markLiveStale() {
    liveSemanticStale = true;
    if (!staleOwners.has(liveStaleOwner) && liveGraceTimerId === void 0) {
      liveGraceTimerId = window.setTimeout(revealLiveStale, LIVE_STALE_GRACE_MS);
    }
    paintStaleState();
  }
  function revealLiveStale() {
    clearLiveGrace();
    staleOwners.add(liveStaleOwner);
    paintStaleState();
  }
  function markLiveUnavailable() {
    clearLiveGrace();
    liveSemanticStale = true;
    staleOwners.add(liveUnavailableOwner);
    paintStaleState();
  }
  function clearLiveStale() {
    clearLiveGrace();
    liveSemanticStale = false;
    staleOwners.delete(liveStaleOwner);
    staleOwners.delete(liveUnavailableOwner);
    paintStaleState();
  }
  function clearListStale() {
    staleOwners.delete(listStaleOwner);
    paintStaleState();
  }
  document.addEventListener("htmx:responseError", (event) => {
    if (isListRefreshEvent(event)) {
      markListStale();
    }
  });
  document.addEventListener("htmx:sendError", (event) => {
    if (isListRefreshEvent(event)) {
      markListStale();
    }
  });

  // internal/assets/src/js/live.ts
  var runtime = {
    status: "off",
    connection: null
  };
  var counters = {
    connections: 0,
    resyncs: 0,
    reconnects: 0,
    v2Snapshots: 0,
    deltas: 0,
    terminals: 0,
    invalidFrames: 0,
    rawBytes: 0,
    payloadBytes: 0,
    snapshotBytes: 0,
    deltaBytes: 0,
    inserted: 0,
    updated: 0,
    deleted: 0,
    projected: 0
  };
  var RESYNC_WINDOW_MS = 3e4;
  var MAX_RESYNCS_PER_WINDOW = 2;
  var LIVE_FIRST_FRAME_TIMEOUT_MS = 3e4;
  var LIVE_READ_IDLE_TIMEOUT_MS = 5e4;
  var completedSnapshotTxns = /* @__PURE__ */ new WeakSet();
  var resyncTimestamps = [];
  var resumeIntent = null;
  var requestSubscribed = false;
  var reconnectTimerId;
  var reconnectAttempt = 0;
  var snapshotAt = 0;
  function addCounter(name, amount = 1) {
    counters[name] += amount;
  }
  function pruneResyncWindow(now = Date.now()) {
    resyncTimestamps = resyncTimestamps.filter((timestamp) => now - timestamp < RESYNC_WINDOW_MS);
  }
  function currentStats() {
    pruneResyncWindow();
    return {
      ...counters,
      state: runtime.status,
      seq: runtime.connection?.cursor?.seq || 0,
      attempt: reconnectAttempt,
      inFlightRequests: listRequestTrackerSnapshot().count,
      resyncsInWindow: resyncTimestamps.length
    };
  }
  function liveCanStreamHere() {
    const content = document.getElementById("resource-list-content");
    if (content?.dataset.liveUrl !== "location") return false;
    return document.querySelector('[data-ro-action="toggle-live"]') !== null;
  }
  function liveStreamBase() {
    return liveStreamBaseForURL(new URL(window.location.href));
  }
  function liveToggleState(status) {
    switch (status) {
      case "open":
        return "open";
      case "reconnecting":
      case "offline":
      case "unavailable":
        return "problem";
      default:
        return "connecting";
    }
  }
  function paintLiveToggleState() {
    paintLiveToggle(runtime.status);
  }
  function paintLiveToggle(status) {
    const toggle = document.querySelector('[data-ro-action="toggle-live"]');
    if (!toggle) return;
    if (status === "off") {
      toggle.removeAttribute("data-ro-live-state");
      return;
    }
    toggle.setAttribute("data-ro-live-state", liveToggleState(status));
  }
  function setStatus(next) {
    runtime.status = next;
    paintLiveToggle(next);
  }
  function isActive(connection) {
    return runtime.connection === connection;
  }
  function connectionToken(source) {
    return Object.freeze({
      ...source,
      cursor: source.cursor ? Object.freeze({ ...source.cursor }) : null
    });
  }
  function replaceConnection(current, cursor) {
    if (!isActive(current)) return null;
    const next = connectionToken({ ...current, cursor });
    runtime.connection = next;
    return next;
  }
  function abortActiveConnection() {
    const connection = runtime.connection;
    runtime.connection = null;
    connection?.ctrl.abort();
  }
  function clearReconnectTimer() {
    window.clearTimeout(reconnectTimerId);
    reconnectTimerId = void 0;
  }
  function liveSetOff() {
    abortActiveConnection();
    clearReconnectTimer();
    resumeIntent = null;
    reconnectAttempt = 0;
    snapshotAt = 0;
    setStatus("off");
    noteStaleRetryAt(0);
    clearLiveStale();
  }
  function liveResetPage() {
    liveSetOff();
    resetListRequestTracker();
    resyncTimestamps = [];
  }
  function enterUnavailable() {
    abortActiveConnection();
    clearReconnectTimer();
    resumeIntent = null;
    setStatus("unavailable");
    noteStaleRetryAt(0);
    markLiveUnavailable();
  }
  function enterDeferred(status, base) {
    resumeIntent = { base };
    setStatus(status);
    noteStaleRetryAt(0);
  }
  function noteDisconnected() {
    clearListValidator();
    markLiveStale();
    if (reconnectAttempt >= 1) revealLiveStale();
  }
  function scheduleReconnect(base, delayMs = null) {
    abortActiveConnection();
    clearReconnectTimer();
    if (!isLiveEnabled()) {
      liveSetOff();
      return;
    }
    if (shouldResetBackoff(snapshotAt, Date.now())) reconnectAttempt = 0;
    noteDisconnected();
    if (!window.navigator.onLine) {
      enterDeferred("offline", base);
      return;
    }
    reconnectAttempt += 1;
    const delay = delayMs ?? reconnectDelayMs(reconnectAttempt);
    setStatus("reconnecting");
    addCounter("reconnects");
    noteStaleRetryAt(Date.now() + delay);
    reconnectTimerId = window.setTimeout(() => {
      reconnectTimerId = void 0;
      if (!isLiveEnabled()) {
        liveSetOff();
        return;
      }
      openConnection(liveCanStreamHere() ? liveStreamBase() : "");
    }, delay);
  }
  function openConnection(base) {
    abortActiveConnection();
    clearReconnectTimer();
    runtime.streamPath = base;
    snapshotAt = 0;
    if (!base) {
      liveSetOff();
      return;
    }
    if (document.hidden) {
      enterDeferred("hidden", base);
      return;
    }
    if (listRequestTrackerSnapshot().count > 0) {
      enterDeferred("suspended", base);
      return;
    }
    if (!window.navigator.onLine) {
      enterDeferred("offline", base);
      return;
    }
    let generation;
    try {
      generation = mintLiveGeneration();
    } catch {
      enterUnavailable();
      return;
    }
    const ctrl = new AbortController();
    const connection = connectionToken({
      ctrl,
      generation,
      base,
      cursor: null
    });
    runtime.connection = connection;
    setStatus("connecting");
    addCounter("connections");
    void liveConnect(connection);
  }
  function responseHeader2(response, name) {
    try {
      return response.headers.get(name);
    } catch {
      return null;
    }
  }
  function acceptsV2Response(response, connection) {
    const contentType = responseHeader2(response, "Content-Type");
    if (contentType?.split(";", 1)[0].trim().toLowerCase() !== "text/event-stream") {
      return false;
    }
    const version = responseHeader2(response, "RO-Live-Version");
    if (version !== null && version !== "2") return false;
    const generation = responseHeader2(response, "RO-Live-Generation");
    return generation === null || generation === connection.generation;
  }
  async function liveConnect(initial) {
    let deadlineTimer = null;
    const clearDeadline = () => {
      if (deadlineTimer !== null) {
        window.clearTimeout(deadlineTimer);
        deadlineTimer = null;
      }
    };
    const armDeadline = (ms) => {
      clearDeadline();
      deadlineTimer = window.setTimeout(() => {
        deadlineTimer = null;
        if (runtime.connection?.ctrl === initial.ctrl) {
          scheduleReconnect(initial.base);
        }
      }, ms);
    };
    armDeadline(LIVE_FIRST_FRAME_TIMEOUT_MS);
    try {
      await runLiveConnection(initial, () => armDeadline(LIVE_READ_IDLE_TIMEOUT_MS));
    } finally {
      clearDeadline();
    }
  }
  function acceptResponse(response, connection) {
    const status = response.status;
    if (status === 429) {
      scheduleReconnect(connection.base, retryAfterMs(responseHeader2(response, "Retry-After")));
      return null;
    }
    if (status === 408) {
      scheduleReconnect(connection.base);
      return null;
    }
    if (status === 204 || status >= 400 && status < 500) {
      enterUnavailable();
      return null;
    }
    if (status !== 200 || !response.body) {
      scheduleReconnect(connection.base);
      return null;
    }
    return response.body;
  }
  async function runLiveConnection(initial, noteLiveProgress) {
    let response;
    try {
      response = await fetch(initial.base, {
        signal: initial.ctrl.signal,
        headers: {
          "RO-Live-Version": "2",
          "RO-Live-Generation": initial.generation
        }
      });
    } catch {
      if (isActive(initial)) scheduleReconnect(initial.base);
      return;
    }
    if (!isActive(initial)) return;
    const body = acceptResponse(response, initial);
    if (!body) return;
    if (!acceptsV2Response(response, initial)) {
      rejectProtocol(initial);
      return;
    }
    const accepted = replaceConnection(initial, null);
    if (!accepted) return;
    let connection = accepted;
    try {
      const reader = body.getReader();
      const parser = new LiveSSEParser();
      let sawFrame = false;
      const readNext = async () => {
        const result = await reader.read();
        if (!isActive(connection) || result.done) return;
        if (sawFrame) noteLiveProgress();
        addCounter("rawBytes", result.value.byteLength);
        let events;
        try {
          events = parser.push(result.value);
        } catch {
          addCounter("invalidFrames");
          rejectProtocol(connection, false);
          return;
        }
        for (const event of events) {
          addCounter("payloadBytes", event.dataBytes);
          handleV2Frame(connection, event.name, event.data, event.dataBytes);
          const current = runtime.connection;
          if (!current || current.ctrl !== connection.ctrl) return;
          connection = current;
          sawFrame = true;
          noteLiveProgress();
        }
        return connection;
      };
      while (await readNext()) {
      }
    } catch {
    }
    if (isActive(connection)) scheduleReconnect(connection.base);
  }
  function handleV2Frame(connection, name, text, payloadBytes) {
    if (name !== "ro-live") {
      rejectProtocol(connection);
      return;
    }
    const decoded = decodeLiveV2Envelope(text);
    if (!decoded.ok) {
      rejectProtocol(connection);
      return;
    }
    const envelope = decoded.value;
    const cursor = connection.cursor;
    if (envelope.g !== connection.generation) {
      rejectProtocol(connection);
      return;
    }
    if (!cursor) {
      if (envelope.kind !== "snapshot" || envelope.seq !== 1) {
        rejectProtocol(connection);
        return;
      }
      commitV2Snapshot(connection, envelope, payloadBytes);
      return;
    }
    if (envelope.kind === "delta") {
      const applied = applyLiveV2Delta(envelope, cursor);
      if (!applied.ok) {
        rejectProtocol(connection);
        return;
      }
      clearListValidator();
      if (!replaceConnection(connection, applied.cursor)) return;
      addCounter("deltas");
      addCounter("deltaBytes", payloadBytes);
      addCounter("inserted", applied.summary.inserted);
      addCounter("updated", applied.summary.updated);
      addCounter("deleted", applied.summary.deleted);
      addCounter("projected", applied.summary.projected);
      setStatus("open");
      return;
    }
    if (envelope.seq !== cursor.seq + 1) {
      rejectProtocol(connection);
      return;
    }
    if (envelope.kind === "snapshot") {
      commitV2Snapshot(connection, envelope, payloadBytes);
      return;
    }
    if (envelope.rev !== cursor.rev || envelope.schema !== cursor.schema) {
      rejectProtocol(connection);
      return;
    }
    addCounter("terminals");
    if (envelope.reason === "auth") {
      enterUnavailable();
      return;
    }
    scheduleReconnect(connection.base);
  }
  function commitV2Snapshot(connection, envelope, payloadBytes) {
    const txn = Object.freeze({});
    swapSnapshot(envelope.snapshot.html, connection, txn);
    if (!completedSnapshotTxns.has(txn) || !isActive(connection)) {
      rejectProtocol(connection);
      return;
    }
    const cursor = {
      g: envelope.g,
      seq: envelope.seq,
      rev: envelope.rev,
      schema: envelope.schema
    };
    if (envelope.rv !== void 0) cursor.rv = envelope.rv;
    replaceConnection(connection, cursor);
    addCounter("v2Snapshots");
    addCounter("snapshotBytes", payloadBytes);
    setStatus("open");
    if (snapshotAt === 0) snapshotAt = Date.now();
    noteStaleRetryAt(0);
    clearLiveStale();
  }
  function swapSnapshot(html, connection, txn) {
    const content = document.getElementById("resource-list-content");
    const htmx2 = window.htmx;
    if (!content || !htmx2 || !isActive(connection)) return;
    clearListValidator();
    const eventInfo = {
      target: content,
      roLivePush: true,
      roLiveSnapshotTxn: txn
    };
    try {
      htmx2.swap(content, html, { swapStyle: "morph" }, { contextElement: content, eventInfo });
    } catch {
    }
  }
  function rejectProtocol(connection, countInvalid = true) {
    if (!isActive(connection)) return;
    if (countInvalid) addCounter("invalidFrames");
    const base = connection.base;
    requestResync(base);
  }
  function requestResync(base) {
    pruneResyncWindow();
    if (resyncTimestamps.length >= MAX_RESYNCS_PER_WINDOW) {
      enterUnavailable();
      return;
    }
    resyncTimestamps.push(Date.now());
    addCounter("resyncs");
    openConnection(base);
  }
  function requestActivity(activity) {
    if (activity.phase === "start") {
      const connection = runtime.connection;
      if (connection) {
        abortActiveConnection();
        enterDeferred(document.hidden ? "hidden" : "suspended", connection.base);
      } else if (runtime.status === "reconnecting" || runtime.status === "offline") {
        clearReconnectTimer();
        enterDeferred(
          document.hidden ? "hidden" : "suspended",
          resumeIntent?.base ?? runtime.streamPath
        );
      }
      return;
    }
    if (!resumeIntent || activity.inFlight !== 0) return;
    if (!isLiveEnabled()) {
      liveSetOff();
      return;
    }
    const { base } = resumeIntent;
    resumeIntent = null;
    openConnection(base);
  }
  function liveOnListSwap(event) {
    const detail = Object(event.detail);
    if (detail.roLivePush !== true) {
      if (resumeIntent) {
        const base = liveCanStreamHere() ? liveStreamBase() : "";
        resumeIntent = { base };
        runtime.streamPath = base;
      }
      return;
    }
    const snapshotTxn = detail.roLiveSnapshotTxn;
    if (typeof snapshotTxn === "object" && snapshotTxn !== null) {
      completedSnapshotTxns.add(snapshotTxn);
    }
  }
  function liveApply(force) {
    if (!requestSubscribed) {
      subscribeListRequests(requestActivity);
      requestSubscribed = true;
    }
    if (!isLiveEnabled()) {
      liveSetOff();
      return;
    }
    const base = liveCanStreamHere() ? liveStreamBase() : "";
    if (force) {
      resyncTimestamps = [];
      resumeIntent = null;
      reconnectAttempt = 0;
      clearReconnectTimer();
      clearLiveStale();
    }
    if (!force && base === runtime.streamPath && runtime.status !== "off") return;
    openConnection(base);
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      const connection = runtime.connection;
      const armed = runtime.status === "reconnecting" || runtime.status === "offline";
      const base = connection?.base ?? (armed ? runtime.streamPath : resumeIntent?.base);
      if (base === void 0) return;
      abortActiveConnection();
      clearReconnectTimer();
      enterDeferred("hidden", base);
      return;
    }
    if (runtime.status === "hidden" && resumeIntent) {
      if (!isLiveEnabled()) {
        liveSetOff();
        return;
      }
      const { base } = resumeIntent;
      resumeIntent = null;
      openConnection(base);
    }
  });
  window.addEventListener("offline", () => {
    const holding = runtime.status === "connecting" || runtime.status === "open" || runtime.status === "reconnecting";
    const base = runtime.connection?.base ?? runtime.streamPath;
    if (!holding || !base) return;
    abortActiveConnection();
    clearReconnectTimer();
    noteDisconnected();
    enterDeferred("offline", base);
  });
  window.addEventListener("online", () => {
    if (runtime.status !== "offline" || !resumeIntent) return;
    if (!isLiveEnabled()) {
      liveSetOff();
      return;
    }
    const { base } = resumeIntent;
    resumeIntent = null;
    openConnection(base);
  });
  window.roLive = { stats: currentStats };

  // internal/assets/src/js/prefs.ts
  var PREFS_MAX_ENCODED = 3072;
  var PREFS_COOKIE_MAX_AGE = 31536e3;
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
    const prefix = "v1.";
    if (!value?.startsWith(prefix)) {
      return { prefs: empty, ok: false };
    }
    const payload = value.slice(prefix.length);
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
    const payload = b64urlEncodeUTF8(`{${fields.join(",")}}`);
    return `v1.${payload}`;
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
    const prefix = "ro_prefs=";
    return document.cookie.split("; ").find((part) => part.startsWith(prefix))?.slice(prefix.length);
  }
  function readPrefs() {
    return decodePrefsValue(prefsCookieValue()).prefs;
  }
  function writePrefs(prefs) {
    try {
      let cookie = "ro_prefs=" + encodePrefsValue(prefs) + "; Path=/; SameSite=Lax; Max-Age=" + PREFS_COOKIE_MAX_AGE;
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
  function getHtmx() {
    return window.htmx;
  }
  var listRequestEpoch = 0;
  var listRequestsInFlight = /* @__PURE__ */ new Map();
  var listRequestSubscribers = /* @__PURE__ */ new Set();
  function requestDetail(event) {
    return Object(event.detail);
  }
  function listRequestTrackerSnapshot() {
    return { count: listRequestsInFlight.size };
  }
  function subscribeListRequests(subscriber) {
    listRequestSubscribers.add(subscriber);
    return () => listRequestSubscribers.delete(subscriber);
  }
  function publishListRequest(phase) {
    const activity = {
      phase,
      inFlight: listRequestsInFlight.size
    };
    listRequestSubscribers.forEach((subscriber) => {
      subscriber(activity);
    });
  }
  function settleListRequest(xhr, owner) {
    const currentOwner = listRequestsInFlight.get(xhr);
    if (currentOwner === void 0 || owner !== void 0 && currentOwner !== owner) return;
    listRequestsInFlight.delete(xhr);
    publishListRequest("settle");
  }
  function trackListRequest(xhr, requestEvent) {
    if (listRequestsInFlight.has(xhr)) return;
    const owner = ++listRequestEpoch;
    listRequestsInFlight.set(xhr, owner);
    xhr.addEventListener("loadend", () => settleListRequest(xhr, owner));
    publishListRequest("start");
    queueMicrotask(() => {
      if (requestEvent.defaultPrevented || xhr.readyState === 0) {
        settleListRequest(xhr, owner);
      }
    });
  }
  function resetListRequestTracker() {
    listRequestEpoch += 1;
    if (listRequestsInFlight.size === 0) return;
    listRequestsInFlight.clear();
    publishListRequest("settle");
  }
  function pruneSettledListRequests() {
    listRequestsInFlight.forEach((owner, xhr) => {
      if (xhr.readyState === 4 || xhr.readyState === 0) settleListRequest(xhr, owner);
    });
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
    const content = document.getElementById("resource-list-content");
    const xhr = detail.xhr;
    if (!content || !(xhr instanceof XMLHttpRequest) || detail.target !== content) return;
    const sourceIsContent = detail.elt === content;
    if (!sourceIsContent && !(detail.elt instanceof Element)) return;
    trackListRequest(xhr, event);
    if (sourceIsContent) return;
    const htmx2 = getHtmx();
    htmx2?.trigger(content, "htmx:abort");
  }
  document.addEventListener("htmx:beforeRequest", handleRefreshBeforeRequest);
  function handleRefreshAfterRequest(event) {
    const xhr = requestDetail(event).xhr;
    if (xhr instanceof XMLHttpRequest) settleListRequest(xhr);
  }
  document.addEventListener("htmx:afterRequest", handleRefreshAfterRequest);
  function listTableURL() {
    const u = new URL(window.location.href);
    return `${u.pathname.replace(/\/+$/, "")}/_table${u.search}`;
  }
  function requestListRefresh() {
    const content = document.getElementById("resource-list-content");
    const htmx2 = getHtmx();
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
  function isLiveEnabled() {
    return readPrefs().refresh === "Live";
  }
  function setLivePreference(on) {
    roPrefsSetRefresh(on ? "Live" : "Off");
  }
  function liveToggleButton() {
    return document.querySelector('[data-ro-action="toggle-live"]');
  }
  function syncLiveToggle() {
    liveToggleButton()?.setAttribute("aria-pressed", isLiveEnabled() ? "true" : "false");
    paintLiveToggleState();
  }
  function syncRefreshNowButton() {
    const button = document.querySelector(
      '[data-ro-action="refresh-now"]'
    );
    if (button) button.disabled = listRequestsInFlight.size > 0;
  }
  subscribeListRequests(syncRefreshNowButton);
  var refreshBindings = [
    // Stale-banner retry: re-fire the (read-only) list GET on
    // #resource-list-content through the shared refresh path (location-backed
    // lists derive `_table` from location.href; multi-type containers trigger
    // their baked ro:refresh). On success the morph swaps fresh rows and the
    // afterSwap handler clears the stale dim + re-hides the banner; on another
    // failure the responseError handler keeps it stale. An in-flight container
    // request is aborted first -- issuing a second container request would make
    // htmx QUEUE it, and a queued request replays on the next htmx:abort with
    // its stale queue-time URL (no queue may ever form). Pure DOM, GET-only --
    // the read-only floor is untouched.
    {
      event: "click",
      selector: '[data-ro-action="retry"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const content = document.getElementById("resource-list-content");
        const htmx2 = getHtmx();
        if (content && htmx2) {
          htmx2.trigger(content, "htmx:abort");
        }
        pruneSettledListRequests();
        if (isLiveEnabled() && liveCanStreamHere()) {
          liveApply(true);
        } else {
          requestListRefresh();
        }
        return true;
      }
    },
    // The Live toggle: the whole update mode is this one boolean. Persist it
    // first (the cookie is what a reload, and the server-rendered aria-pressed,
    // read), then hand the transport its instruction -- liveApply(true) forces
    // a fresh attempt even from a previously failed state, liveSetOff aborts
    // and clears the warning surface without issuing any request. The toggle
    // renders only where the server said `_stream` answers.
    {
      event: "click",
      selector: '[data-ro-action="toggle-live"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        const on = !isLiveEnabled();
        setLivePreference(on);
        syncLiveToggle();
        if (on) {
          liveApply(true);
        } else {
          liveSetOff();
        }
        return true;
      }
    },
    // Refresh now: EXACTLY one `_table` request per click, no timer armed, no
    // preference written. The disabled paint is defence in depth -- a click
    // arriving while the tracker is occupied (a queued keyboard repeat, a
    // synthetic click) must not stack a second container request. Prune first:
    // this click is the one gate that can rescue a tracker entry whose issuing
    // element detached mid-request (its htmx:afterRequest never bubbled), so a
    // swallowed terminal event cannot disable Refresh until a hard reload.
    {
      event: "click",
      selector: '[data-ro-action="refresh-now"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        pruneSettledListRequests();
        if (listRequestsInFlight.size === 0) {
          requestListRefresh();
        }
        syncRefreshNowButton();
        return true;
      }
    },
    // The Unavailable banner's Reload: this session can no longer stream (an
    // auth terminal or a rejected admission), and no in-page retry can fix it.
    // A full document load is the only recovery, so it is the only action.
    {
      event: "click",
      selector: '[data-ro-action="reload"]',
      stop: true,
      handler: (event) => {
        event.preventDefault();
        window.location.reload();
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
  function applyLiveRowDeletions(keys) {
    let changed = false;
    for (const key of keys) changed = rowSelection.delete(key) || changed;
    if (rowFocusKey !== null && keys.has(rowFocusKey)) {
      rowFocusKey = null;
      changed = true;
    }
    if (changed) updateBulkBar();
  }
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
  function roRowState() {
    return window.roRowState;
  }
  var virtState = {
    active: false,
    visible: [],
    rowH: 0,
    start: 0,
    end: 0,
    table: null,
    tbody: null,
    topSpacer: null,
    bottomSpacer: null,
    pinnedWidths: [],
    pendingScrollY: null
  };
  var historyRecoveryPending = null;
  function virtualizerActive() {
    return virtState.active && virtState.tbody?.isConnected === true;
  }
  function virtReset() {
    virtState.active = false;
    virtState.visible = [];
    virtState.rowH = 0;
    virtState.start = 0;
    virtState.end = 0;
    virtState.table = null;
    virtState.tbody = null;
    virtState.topSpacer = null;
    virtState.bottomSpacer = null;
    virtState.pinnedWidths = [];
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
    virtState.visible = listProjectionVisibleRows();
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
    if (!content) {
      resetListProjection();
      virtReset();
      return;
    }
    if (!wrap) {
      ensureListProjection(content);
      virtReset();
      return;
    }
    const table = wrap.querySelector("table.ro-table");
    const tbody = table?.tBodies.item(0) ?? null;
    if (!tbody) {
      ensureListProjection(content);
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
      ensureListProjection(content);
      historyRecoveryPending = { content, tbody };
      clearListValidator();
      requestListRefresh();
      return;
    }
    ensureListProjection(content);
    const rows = listProjectionRows();
    if (rows.length === 0) {
      virtReset();
      return;
    }
    historyRecoveryPending = null;
    virtState.pendingScrollY = null;
    virtState.table = table;
    virtState.tbody = tbody;
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
    virtState.pendingScrollY = null;
    const incoming = prepareListProjectionSwap(fragment);
    if (!incoming.windowed || incoming.rows.length === 0) {
      return;
    }
    const wrap = fragment.querySelector(".ro-table-wrap.ro-windowed");
    const tbody = wrap ? wrap.querySelector("table.ro-table tbody") : null;
    if (!tbody) {
      return;
    }
    virtState.pendingScrollY = window.scrollY;
    const rowH = virtState.rowH || virtFallbackRowHeight();
    const priorStart = virtState.active ? virtState.start : 0;
    const heights = prepareSwapSpacers(priorStart, incoming.rows.length, rowH);
    const topSpacer = virtMakeSpacer();
    const bottomSpacer = virtMakeSpacer();
    topSpacer.firstElementChild.style.height = `${heights.top}px`;
    bottomSpacer.firstElementChild.style.height = `${heights.bottom}px`;
    tbody.replaceChildren(topSpacer, bottomSpacer);
  }
  function virtualizeAfterSwap() {
    historyRecoveryPending = null;
    const wasActive = virtState.active;
    const previousByKey = commitListProjectionSwap();
    if (!previousByKey || !listProjectionWindowed() || listProjectionRows().length === 0) {
      virtReset();
      return;
    }
    if (!virtBindMounts()) {
      resetListProjection();
      virtReset();
      return;
    }
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
    virtFlashChangedCells(previousByKey);
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
    if (!virtualizerActive() || listProjectionSwapPending()) {
      return;
    }
    virtComputeVisible();
    virtRenderWindow();
  }
  function virtualizeAfterDelta(previousByKey, focusKey = null) {
    if (!virtualizerActive()) return;
    if (focusKey) virtualizeRevealKey(focusKey);
    virtFlashChangedCells(previousByKey);
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
    if (delta === 0) return;
    window.scrollBy(0, delta);
    virtRenderWindow();
  }
  function virtualizeRevealKey(key) {
    if (!virtualizerActive()) return false;
    const index = virtState.visible.findIndex((row2) => row2.dataset.key === key);
    if (index === -1) return false;
    const row = virtState.visible[index];
    virtualizeScrollToIndex(index);
    return row.isConnected;
  }
  function virtRows() {
    return virtualizerActive() ? Array.from(listProjectionRows()) : [];
  }
  function virtVisible() {
    return virtState.visible;
  }
  function virtRowByKey(key) {
    return virtualizerActive() ? listProjectionRowByKey(key) : null;
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
      const tr = listProjectionRowByKey(key);
      const index = tr ? virtState.visible.indexOf(tr) : -1;
      if (index === -1) {
        return false;
      }
      virtualizeScrollToIndex(index);
      return true;
    }
  };

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
        const listTarget = target.id === "resource-list-content";
        if (listTarget) {
          prepareListProjectionSwap(fragment);
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

  // internal/assets/src/js/filters.ts
  function getHtmx2() {
    return window.htmx;
  }
  var roRowModel = listProjectionRowModel();
  function captureRowModelFromDocument() {
    const content = document.getElementById("resource-list-content");
    if (content && !virtualizerActive()) {
      ensureListProjection(content);
    }
  }
  var FILTER_HIDE_CLASS2 = "ro-row-filtered";
  var appliedLiveFilter = null;
  function applyLiveNameFilter() {
    const content = document.getElementById("resource-list-content");
    if (!content) {
      return;
    }
    const input = document.getElementById("ro-filter-input");
    const draft = input ? input.value : "";
    const revision = listProjectionRevision();
    if (appliedLiveFilter?.content === content && appliedLiveFilter.draft === draft && appliedLiveFilter.revision === revision) {
      return;
    }
    const visible = liveNameMatchKeys(roRowModel.rows, draft);
    setListProjectionVisibleKeys(visible);
    content.querySelectorAll("tbody tr[data-key], .ro-cardlist > .ro-pcard[data-key]").forEach((item) => {
      item.classList.toggle(
        FILTER_HIDE_CLASS2,
        !!visible && !visible.has(item.dataset.key)
      );
    });
    virtualizeOnFilterChange();
    appliedLiveFilter = { content, draft, revision };
  }
  function issueFilterNavigation(href) {
    const content = document.getElementById("resource-list-content");
    const input = document.getElementById("ro-filter-input");
    const htmx2 = getHtmx2();
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
    if (!filterFieldKnown(roRowModel.fields, parsed.field)) {
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
    const names = filterSuggestionFields(roRowModel.fields).slice(0, 3).map((f) => f.text);
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
      openFilterAC(rankFieldSuggestions(roRowModel.fields, draft));
      return;
    }
    if (parsed.op !== ":" || !filterFieldKnown(roRowModel.fields, parsed.field)) {
      closeFilterAC();
      return;
    }
    openFilterAC(rankValueSuggestions(roRowModel.fields, roRowModel.rows, parsed));
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

  // internal/assets/src/js/columns.ts
  function getHtmx3() {
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
    const htmx2 = getHtmx3();
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
    {
      event: "click",
      selector: "[data-ro-cols-toggle]",
      handler: (event) => {
        event.preventDefault();
        const pop = document.getElementById("ro-cols-pop");
        setColsPopOpen(!!pop && !pop.classList.contains("is-open"));
      }
    },
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
    {
      event: "input",
      selector: "#ro-palette-input",
      stop: true,
      handler: (_event, matched) => {
        renderPalette(matched.value);
        return true;
      }
    },
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
    // refresh-domain tails LAST: the retry hook and the navbar update controls
    // (toggle-live / refresh-now / the Unavailable banner's reload) were the
    // monolith big click listener's own trailing branches, so registering them
    // after the migrated leaves preserves the C1 order -- every leaf front-ran
    // the monolith, and these ran at its end. None co-matches any selector
    // above, so the position is observationally free; LAST documents their
    // monolith origin.
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
  function suppressRedundantActiveNavigation(event) {
    const detail = Object(event.detail);
    const source = detail.elt;
    const trigger = detail.triggeringEvent;
    if (detail.boosted !== true || detail.target !== document.body || detail.verb !== "get" || !(source instanceof HTMLAnchorElement) || !(trigger instanceof MouseEvent) || trigger.type !== "click" || trigger.button !== 0 || trigger.altKey || trigger.ctrlKey || trigger.metaKey || trigger.shiftKey) {
      return;
    }
    if (!source.classList.contains("is-active") && !source.parentElement?.classList.contains("is-active")) {
      return;
    }
    const rawHref = source.getAttribute("href");
    if (!rawHref || rawHref.includes("#") || typeof detail.path !== "string") {
      return;
    }
    try {
      const current = new URL(window.location.href);
      const resolvesToCurrentLocation = (candidate) => {
        const resolved = new URL(candidate, current);
        return resolved.origin === current.origin && resolved.pathname === current.pathname && resolved.search === current.search && resolved.hash === current.hash;
      };
      if (resolvesToCurrentLocation(rawHref) && resolvesToCurrentLocation(detail.path)) {
        event.preventDefault();
      }
    } catch {
    }
  }
  document.addEventListener("htmx:configRequest", suppressRedundantActiveNavigation);
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
    if (cfg.headers["RO-No-Push"]) {
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
  function refreshFilterAutocomplete() {
    const input = document.getElementById("ro-filter-input");
    if (input && document.activeElement === input && input.value) updateFilterAC();
  }
  function restoreColumnsPopover() {
    if (colsPopOpen()) setColsPopOpen(true);
  }
  function afterListUpdate(update) {
    if (update.kind === "swap") {
      runInitStep(() => rememberListValidator(update.event));
    } else {
      runInitStep(() => applyLiveRowDeletions(update.deletedKeys));
    }
    [
      clearListStale,
      reapplyRowState,
      applyLiveNameFilter,
      refreshFilterAutocomplete,
      restoreColumnsPopover
    ].forEach(runInitStep);
    if (update.kind === "swap") runInitStep(virtualizeAfterSwap);
    else runInitStep(() => virtualizeAfterDelta(update.previousByKey, update.focusKey));
    runInitStep(setupStickyNamespace);
  }
  document.addEventListener(LIST_DELTA_APPLIED_EVENT, (event) => {
    const detail = event.detail;
    afterListUpdate(detail);
  });
  document.addEventListener("htmx:afterSwap", (event) => {
    const bodySwapped = event.target === document.body;
    if (bodySwapTicket && bodySwapped) {
      if (bodySwapTicket.phase === "swap") {
        completeBodySwap();
      } else {
        reloadCurrentHistoryEntry();
      }
    }
    if (isListRefreshEvent(event)) {
      try {
        afterListUpdate({ kind: "swap", event });
      } finally {
        liveOnListSwap(event);
      }
    }
    if (bodySwapped && !bodyReloading) {
      bodyInitPending = document.body;
      runInitStep(buildYamlFolds);
    }
  });
  document.addEventListener("htmx:beforeSwap", (event) => {
    const detail = event.detail;
    if (suppressListNotModified(event)) {
      clearListStale();
      return;
    }
    if (detail && detail.target === document.body) {
      if (bodySwapTicket || bodyReloading) {
        event.preventDefault();
        reloadCurrentHistoryEntry();
        return;
      }
      const status = detail.xhr?.status;
      if (typeof status === "number" && status >= 400 && status <= 599) {
        detail.shouldSwap = true;
      }
      const ticket = claimBodySwap("normal", "swap", null);
      closeRowMenu();
      clearRowState();
      clearListStale();
      liveResetPage();
      queueMicrotask(() => {
        if (!event.defaultPrevented && detail.shouldSwap !== false) return;
        reloadFailedBodySwap(ticket);
      });
    }
  });
  var bodySwapTicket = null;
  var bodyReloading;
  var bodyInitPending = null;
  function clearBodySwap() {
    bodySwapTicket = null;
  }
  function completeBodySwap() {
    clearBodySwap();
    bodyReloading = void 0;
  }
  function retireCurrentScreenForBodySwap() {
    clearListStale();
    liveResetPage();
  }
  function reloadCurrentHistoryEntry() {
    if (bodyReloading) return;
    if (!bodySwapTicket) retireCurrentScreenForBodySwap();
    clearBodySwap();
    bodyReloading = true;
    window.history.go(0);
  }
  function reloadFailedBodySwap(ticket) {
    if (!ticket || bodySwapTicket !== ticket) return;
    reloadCurrentHistoryEntry();
  }
  function claimBodySwap(kind, phase, xhr) {
    const ticket = { kind, phase, xhr };
    bodySwapTicket = ticket;
    return ticket;
  }
  function beginHistoryBodySwap(event) {
    if (bodySwapTicket || bodyReloading) {
      event.preventDefault();
      reloadCurrentHistoryEntry();
      return;
    }
    const miss = event.type === "htmx:historyCacheMiss";
    const detail = Object(event.detail);
    const xhr = miss && detail.xhr instanceof EventTarget ? detail.xhr : null;
    const ticket = claimBodySwap(miss ? "miss" : "hit", miss ? "request" : "swap", xhr);
    retireCurrentScreenForBodySwap();
    queueMicrotask(() => {
      if (event.defaultPrevented) reloadFailedBodySwap(ticket);
    });
    if (miss) {
      if (!xhr) {
        event.preventDefault();
        return;
      }
      xhr.addEventListener("loadend", () => {
        if (ticket.phase === "request") {
          reloadFailedBodySwap(ticket);
        }
      });
    }
  }
  document.addEventListener("htmx:historyCacheHit", beginHistoryBodySwap);
  document.addEventListener("htmx:historyCacheMiss", beginHistoryBodySwap);
  document.addEventListener("htmx:historyCacheMissLoad", (event) => {
    const detail = Object(event.detail);
    const ticket = bodySwapTicket;
    if (ticket?.kind !== "miss" || ticket.phase !== "request" || detail.xhr !== ticket.xhr) {
      event.preventDefault();
      reloadCurrentHistoryEntry();
      return;
    }
    ticket.phase = "swap";
  });
  document.addEventListener("htmx:historyCacheMissLoadError", reloadCurrentHistoryEntry);
  document.addEventListener("htmx:swapError", (event) => {
    const detail = Object(event.detail);
    if (event.target === document.body || detail.target === document.body) {
      reloadFailedBodySwap(bodySwapTicket);
    }
  });
  document.addEventListener("htmx:historyRestore", () => {
  });
  window.addEventListener("pageshow", () => {
    completeBodySwap();
    bodyInitPending = null;
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
  function runInit(yamlFoldsBuilt = false) {
    if (bodySwapTicket || bodyReloading) return;
    const steps = [
      syncLiveToggle,
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
      // A new projection deliberately clears stale visibleKeys. Re-derive
      // them from the current draft before windowing so navigation/history
      // cannot carry an old page's filter set into this one.
      applyLiveNameFilter,
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
      updateBulkBar,
      // Live opens only after every synchronous body/model repair, and is the
      // LAST step for that reason. In particular, virtualizeInit may detect a
      // history-restored viewport slice and synchronously issue the mandatory
      // full `_table` rebuild; its beforeRequest ownership must exist before
      // liveApply decides whether to open or suspend.
      liveApply
    ];
    if (!yamlFoldsBuilt) steps.splice(1, 0, buildYamlFolds);
    steps.forEach(runInitStep);
  }
  document.addEventListener("DOMContentLoaded", () => runInit());
  document.addEventListener("htmx:afterSettle", (event) => {
    const pending = bodyInitPending;
    const detail = Object(event.detail);
    if (pending && event.target !== pending && detail.target !== pending) {
      return;
    }
    if (!pending) {
      if (!isListRefreshEvent(event)) setupStickyNamespace();
      return;
    }
    bodyInitPending = null;
    runInit(true);
  });
  window.addEventListener("resize", setupStickyNamespace);
})();
