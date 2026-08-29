// live-url.ts -- byte-preserving Live URL identity and generation helpers.

const CLIENT_GENERATION = /^(?:[0-9a-f]{32}|[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})$/iu;

export function isClientLiveGeneration(value: unknown): value is string {
    return typeof value === 'string' && value.length <= 64 && CLIENT_GENERATION.test(value);
}

export function mintLiveGeneration(cryptoSource: Crypto = window.crypto): string {
    try {
        const uuid = cryptoSource.randomUUID();
        if (isClientLiveGeneration(uuid)) return uuid;
    } catch {
        // Older WebCrypto implementations expose getRandomValues but not UUID.
    }
    const bytes = cryptoSource.getRandomValues(new Uint8Array(16));
    const generation = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
    if (!isClientLiveGeneration(generation)) throw new Error('invalid Live generation');
    return generation;
}

function decodedQueryKey(raw: string): string | null {
    try {
        return decodeURIComponent(raw.replaceAll('+', ' '));
    } catch {
        return null;
    }
}

// Remove every decoded `g` key while retaining every surviving pair byte for
// byte (including malformed escapes, bare keys, duplicates, and empty pairs).
export function stripLiveGenerationQuery(rawQuery: string): string {
    if (rawQuery === '') return '';
    return rawQuery
        .split('&')
        .filter((pair) => decodedQueryKey(pair.split('=', 1)[0]) !== 'g')
        .join('&');
}

function withRawQuery(pathname: string, rawQuery: string): string {
    return rawQuery === '' ? pathname : `${pathname}?${rawQuery}`;
}

export function liveStreamBaseForURL(url: URL): string {
    const pathname = `${url.pathname.replace(/\/+$/, '')}/_stream`;
    return withRawQuery(pathname, stripLiveGenerationQuery(url.search.slice(1)));
}

export function liveScreenForBase(base: string): string {
    const queryStart = base.indexOf('?');
    const pathname = queryStart < 0 ? base : base.slice(0, queryStart);
    const query = queryStart < 0 ? '' : base.slice(queryStart + 1);
    const screenPath = pathname.endsWith('/_stream')
        ? pathname.slice(0, -'/_stream'.length)
        : pathname;
    return withRawQuery(screenPath, query);
}

export function liveRequestURL(base: string, generation: string): string {
    if (!isClientLiveGeneration(generation)) throw new Error('invalid Live generation');
    return `${base}${base.includes('?') ? '&' : '?'}g=${generation}`;
}

export function liveStreamBaseFromTableRequest(requestPath: unknown): string | null {
    if (typeof requestPath !== 'string') return null;
    const queryStart = requestPath.indexOf('?');
    const pathname = queryStart < 0 ? requestPath : requestPath.slice(0, queryStart);
    if (!pathname.endsWith('/_table')) return null;
    const rawQuery = queryStart < 0 ? '' : requestPath.slice(queryStart + 1);
    return withRawQuery(
        `${pathname.slice(0, -'/_table'.length)}/_stream`,
        stripLiveGenerationQuery(rawQuery),
    );
}
