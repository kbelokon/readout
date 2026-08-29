// live-url.ts -- byte-preserving Live URL identity and generation helpers.

const CLIENT_GENERATION_HEX_LENGTH = 32;
const CLIENT_GENERATION_UUID_LENGTH = 36;
const CLIENT_GENERATION_UUID_DASHES = new Set([8, 13, 18, 23]);
const ASCII_HEX_DIGITS = '0123456789abcdefABCDEF';

export function isClientLiveGeneration(value: unknown): value is string {
    if (
        typeof value !== 'string' ||
        (value.length !== CLIENT_GENERATION_HEX_LENGTH &&
            value.length !== CLIENT_GENERATION_UUID_LENGTH)
    ) {
        return false;
    }
    const uuid = value.length === CLIENT_GENERATION_UUID_LENGTH;
    return Array.from(value).every((character, index) =>
        uuid && CLIENT_GENERATION_UUID_DASHES.has(index)
            ? character === '-'
            : ASCII_HEX_DIGITS.includes(character),
    );
}

export function mintLiveGeneration(cryptoSource: Crypto = window.crypto): string {
    try {
        const uuid = cryptoSource.randomUUID();
        if (isClientLiveGeneration(uuid)) return uuid;
    } catch {
        // Older WebCrypto implementations expose getRandomValues but not UUID.
    }
    const bytes = cryptoSource.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
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
    return rawQuery
        .split('&')
        .filter((pair) => decodedQueryKey(pair.split('=', 1)[0]) !== 'g')
        .join('&');
}

function withRawQuery(pathname: string, rawQuery: string): string {
    return rawQuery === '' ? pathname : `${pathname}?${rawQuery}`;
}

function splitRawQuery(path: string): [pathname: string, rawQuery: string] {
    const queryStart = path.indexOf('?');
    return queryStart === -1 ? [path, ''] : [path.slice(0, queryStart), path.slice(queryStart + 1)];
}

export function liveStreamBaseForURL(url: URL): string {
    const pathname = `${url.pathname.replace(/\/+$/, '')}/_stream`;
    return withRawQuery(pathname, stripLiveGenerationQuery(url.search.slice(1)));
}

export function liveScreenForBase(base: string): string {
    const [pathname, query] = splitRawQuery(base);
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
    try {
        const url = new URL(requestPath, window.location.href);
        if (url.origin !== window.location.origin || !url.pathname.endsWith('/_table')) return null;
        return withRawQuery(
            `${url.pathname.slice(0, -'/_table'.length)}/_stream`,
            stripLiveGenerationQuery(url.search.slice(1)),
        );
    } catch {
        return null;
    }
}
