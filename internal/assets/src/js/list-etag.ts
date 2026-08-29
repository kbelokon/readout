// list-etag.ts -- the app-managed validator for resource-list `_table`
// representations. Browser caching stays disabled (`Cache-Control: no-store`):
// the last-good DOM is the representation cache, and this module conditionally
// asks the server whether that exact DOM is still current.
//
// The validator pair lives on the persistent #resource-list-content element,
// not in module globals. An innerHTML morph keeps the pair; a boosted body swap
// replaces the element and therefore drops the old screen's validator without
// a separate teardown path.

const LIST_CONTENT_ID = 'resource-list-content';
const ETAG_DATA_KEY = 'roEtag';
const PATH_DATA_KEY = 'roEtagPath';

interface HtmxDetail {
    elt?: unknown;
    headers?: unknown;
    isError?: unknown;
    path?: unknown;
    pathInfo?: unknown;
    requestConfig?: unknown;
    roLivePush?: unknown;
    shouldSwap?: unknown;
    target?: unknown;
    xhr?: unknown;
}

interface ListValidator {
    etag: string;
    path: string;
}

function eventDetail(event: Event): HtmxDetail {
    return Object((event as CustomEvent).detail) as HtmxDetail;
}

function currentListContent(): HTMLElement | null {
    return document.getElementById(LIST_CONTENT_ID);
}

// ETag is an RFC 9110 entity-tag: an optional, case-sensitive W/ prefix and a
// quoted opaque tag. The server emits printable ASCII base64url tags, so reject
// controls / obs-text here instead of carrying an untrustworthy response value
// back into a request header.
function validETag(value: unknown): value is string {
    return typeof value === 'string' && /^(?:W\/)?"[\x21\x23-\x7e]*"$/.test(value);
}

// The request identity is the exact same-origin wire path and query. A hash is
// never sent over HTTP and therefore is not part of a representation key.
// Query order and encoding remain significant: URL does not round-trip through
// URLSearchParams.
function tableRequestKey(value: unknown): string | null {
    if (typeof value !== 'string' || value.length === 0) {
        return null;
    }
    try {
        const url = new URL(value, window.location.href);
        if (url.origin !== window.location.origin || !url.pathname.endsWith('/_table')) {
            return null;
        }
        return `${url.pathname}${url.search}`;
    } catch {
        return null;
    }
}

function headerRecord(value: unknown): Record<string, unknown> {
    return Object(value) as Record<string, unknown>;
}

function headerValue(headers: unknown, wanted: string): string | null {
    const lowerWanted = wanted.toLowerCase();
    for (const [name, value] of Object.entries(headerRecord(headers))) {
        if (name.toLowerCase() === lowerWanted && typeof value === 'string') {
            return value;
        }
    }
    return null;
}

function deleteHeader(headers: unknown, unwanted: string): void {
    const record = headerRecord(headers);
    const lowerUnwanted = unwanted.toLowerCase();
    for (const name of Object.keys(record)) {
        if (name.toLowerCase() === lowerUnwanted) {
            delete record[name];
        }
    }
}

function responseHeader(xhr: unknown, name: string): string | null {
    const candidate = Object(xhr) as { getResponseHeader?: unknown };
    if (typeof candidate.getResponseHeader !== 'function') {
        return null;
    }
    try {
        const value = candidate.getResponseHeader.call(xhr, name) as unknown;
        return typeof value === 'string' ? value : null;
    } catch {
        return null;
    }
}

function clearContentValidator(content: HTMLElement): void {
    delete content.dataset[ETAG_DATA_KEY];
    delete content.dataset[PATH_DATA_KEY];
}

function readContentValidator(content: HTMLElement): ListValidator | null {
    const etag = content.dataset[ETAG_DATA_KEY];
    const path = content.dataset[PATH_DATA_KEY];
    if (!validETag(etag) || tableRequestKey(path) !== path) {
        // A half-written or externally corrupted pair must never become a
        // conditional request. Clear both halves so later reads stay atomic.
        clearContentValidator(content);
        return null;
    }
    return { etag, path };
}

function writeContentValidator(content: HTMLElement, validator: ListValidator): void {
    clearContentValidator(content);
    content.dataset[PATH_DATA_KEY] = validator.path;
    content.dataset[ETAG_DATA_KEY] = validator.etag;
}

// configureListValidatorRequest runs during htmx:configRequest, after
// refresh.ts has marked a current-container request RO-No-Push. It strips every
// case spelling of a caller-supplied If-None-Match from list traffic, then adds
// the app-owned value only for the exact persistent-container + URL pair. A
// deliberately emptied current container cannot represent the stored entity:
// clear both validator halves before its refresh so a 304 cannot preserve the
// blank/skeleton region.
// User sort/filter requests target the container but are sourced elsewhere, so
// they remain unconditional.
export function configureListValidatorRequest(event: Event): void {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content) {
        return;
    }
    const sourceIsContent = detail.elt === content;
    const targetIsContent = detail.target === content;
    if (!sourceIsContent && !targetIsContent) {
        return; // unrelated traffic: do not rewrite application-owned headers
    }

    const headers = headerRecord(detail.headers);
    deleteHeader(headers, 'If-None-Match');

    // Real htmx config events always carry target. Accept an absent target for
    // direct/unit invocation, but never a distinct supplied target.
    if (
        !sourceIsContent ||
        (detail.target !== undefined && !targetIsContent) ||
        headerValue(headers, 'RO-No-Push') !== 'true' ||
        headerValue(headers, 'HX-Preloaded') === 'true'
    ) {
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
    headers['If-None-Match'] = validator.etag;
}

// rememberListValidator runs first in the successful list afterSwap pipeline.
// Every real `_table` 200 either replaces the pair with trustworthy metadata or
// clears it. Errors never reach afterSwap and intentionally retain the
// last-good pair, allowing an unchanged recovery request to return 304.
export function rememberListValidator(event: Event): void {
    const detail = eventDetail(event);
    const content = currentListContent();
    if (!content || detail.target !== content) {
        return;
    }
    if (detail.roLivePush === true) {
        clearContentValidator(content);
        return;
    }

    const xhr = Object(detail.xhr) as { status?: unknown };
    if (xhr.status !== 200) {
        return;
    }
    const pathInfo = Object(detail.pathInfo) as { finalRequestPath?: unknown };
    const path = tableRequestKey(pathInfo.finalRequestPath);
    const etag = responseHeader(detail.xhr, 'ETag');
    if (!path || !validETag(etag)) {
        clearContentValidator(content);
        return;
    }
    writeContentValidator(content, { etag, path });
}

// clearListValidator invalidates the current screen synchronously. Live calls
// it immediately before accepting a pushed morph so a later fallback poll must
// fetch one full 200 snapshot before conditional refreshes resume.
export function clearListValidator(): void {
    const content = currentListContent();
    if (content) {
        clearContentValidator(content);
    }
}

// suppressListNotModified fail-safes EVERY 304 targeting the current list:
// htmx 2.0 treats every 3xx as swapping by default, but a 304 has no body and
// must never erase the last-good table. Only the exact conditional request this
// module issued (including the repeated response ETag) is a successful recovery.
// An unmatched current-list 304 remains non-swapping but is marked as an error,
// so htmx emits responseError and the ordinary stale/backoff path takes over.
export function suppressListNotModified(event: Event): boolean {
    const detail = eventDetail(event);
    const content = currentListContent();
    const xhr = Object(detail.xhr) as { status?: unknown };
    if (!content || detail.target !== content || xhr.status !== 304) {
        return false;
    }
    detail.shouldSwap = false;
    detail.isError = true;

    const validator = readContentValidator(content);
    // In htmx:beforeSwap, top-level detail.elt is the event dispatch target,
    // not necessarily the element that issued the request. htmx preserves the
    // issuing element on requestConfig.elt; exact recovery must prove that one.
    const config = Object(detail.requestConfig) as { elt?: unknown; headers?: unknown };
    const pathInfo = Object(detail.pathInfo) as { finalRequestPath?: unknown };
    if (
        config.elt !== content ||
        !validator ||
        headerValue(config.headers, 'RO-No-Push') !== 'true' ||
        headerValue(config.headers, 'If-None-Match') !== validator.etag ||
        tableRequestKey(pathInfo.finalRequestPath) !== validator.path ||
        responseHeader(detail.xhr, 'ETag') !== validator.etag
    ) {
        return false;
    }
    detail.isError = false;
    return true;
}
