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

function withRawQuery(pathname: string, rawQuery: string): string {
    return rawQuery === '' ? pathname : `${pathname}?${rawQuery}`;
}

// The stream target is always same-origin. Asserting it here (rather than
// trusting that a pathname can never start with `//`) keeps a protocol-relative
// path from turning into a cross-origin fetch of the viewer's live data.
export function liveStreamBaseForURL(url: URL): string {
    if (url.origin !== window.location.origin) return '';
    const pathname = `${url.pathname.replace(/\/+$/, '')}/_stream`;
    if (!pathname.startsWith('/') || pathname.startsWith('//')) return '';
    return withRawQuery(pathname, url.search.slice(1));
}
