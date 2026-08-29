// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import {
    clearListValidator,
    configureListValidatorRequest,
    rememberListValidator,
    suppressListNotModified,
} from './list-etag.js';

const ETAG = 'W/"ro-list-v1-dGVzdA"';
const OTHER_ETAG = 'W/"ro-list-v1-b3RoZXI"';
const TABLE_PATH = '/clusters/prod/namespaces/default/pods/_table?sort=Name&f=status%3ARunning';

function renderContent(): HTMLElement {
    const content = document.createElement('div');
    content.id = 'resource-list-content';
    content.append(document.createElement('table'));
    document.body.appendChild(content);
    return content;
}

function responseXHR(status: number, etag: string | null): XMLHttpRequest {
    return {
        status,
        getResponseHeader: vi.fn((name: string) => (name.toLowerCase() === 'etag' ? etag : null)),
    } as unknown as XMLHttpRequest;
}

function htmxEvent(type: string, detail: unknown): CustomEvent {
    return new CustomEvent(type, { bubbles: true, detail });
}

function remember(
    content: HTMLElement,
    options: { etag?: string | null; path?: unknown; status?: number } = {},
): void {
    rememberListValidator(
        htmxEvent('htmx:afterSwap', {
            target: content,
            xhr: responseXHR(
                options.status ?? 200,
                Object.hasOwn(options, 'etag') ? (options.etag as string | null) : ETAG,
            ),
            pathInfo: {
                finalRequestPath: Object.hasOwn(options, 'path') ? options.path : TABLE_PATH,
            },
        }),
    );
}

function conditionalDetail(content: HTMLElement): Record<string, unknown> {
    return {
        elt: content,
        target: content,
        xhr: responseXHR(304, ETAG),
        requestConfig: {
            elt: content,
            headers: { 'ro-no-push': 'true', 'IF-NONE-MATCH': ETAG },
        },
        pathInfo: { finalRequestPath: TABLE_PATH },
        shouldSwap: true,
        isError: true,
    };
}

beforeEach(() => {
    window.history.replaceState(null, '', '/clusters/prod/namespaces/default/pods');
});

describe('successful representation storage', () => {
    test.each([ETAG, '"strong-tag"', 'W/""'])(
        'remembers a valid %s ETag with its exact path',
        (etag) => {
            const content = renderContent();

            remember(content, { etag });

            expect(content.dataset.roEtag).toBe(etag);
            expect(content.dataset.roEtagPath).toBe(TABLE_PATH);
        },
    );

    test('accepts a same-origin absolute final URL but stores only pathname and search', () => {
        const content = renderContent();

        remember(content, {
            path: `https://readout.test${TABLE_PATH}#not-on-the-wire`,
        });

        expect(content.dataset.roEtag).toBe(ETAG);
        expect(content.dataset.roEtagPath).toBe(TABLE_PATH);
    });

    test('rejects an empty final path even when the current page itself ends in _table', () => {
        const content = renderContent();
        remember(content);
        window.history.replaceState(null, '', TABLE_PATH);

        remember(content, { path: '' });

        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });

    test.each([
        ['missing ETag', null, TABLE_PATH],
        ['malformed ETag', 'W/ "space-before-tag"', TABLE_PATH],
        ['multiple ETags', `${ETAG}, ${OTHER_ETAG}`, TABLE_PATH],
        ['missing path', ETAG, undefined],
        ['cross-origin path', ETAG, `https://attacker.test${TABLE_PATH}`],
        ['non-table path', ETAG, '/clusters/prod/pods'],
        ['table-looking query only', ETAG, '/clusters/prod/pods?next=/_table'],
    ])('a 200 with $0 clears the complete prior pair', (_name, etag, path) => {
        const content = renderContent();
        remember(content);

        remember(content, { etag, path });

        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });

    test('errors and events for a detached same-id target retain the last-good pair', () => {
        const content = renderContent();
        remember(content);
        const detached = document.createElement('div');
        detached.id = 'resource-list-content';

        remember(content, { etag: null, status: 500 });
        rememberListValidator(
            htmxEvent('htmx:afterSwap', {
                target: detached,
                xhr: responseXHR(200, OTHER_ETAG),
                pathInfo: { finalRequestPath: '/other/_table' },
            }),
        );

        expect(content.dataset.roEtag).toBe(ETAG);
        expect(content.dataset.roEtagPath).toBe(TABLE_PATH);
    });

    test('a synthetic Live push clears the pair even without XHR metadata', () => {
        const content = renderContent();
        remember(content);

        rememberListValidator(htmxEvent('htmx:afterSwap', { target: content, roLivePush: true }));

        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });
});

describe('configRequest conditional header gate', () => {
    test('replaces every spoofed spelling with the stored validator for the exact container URL', () => {
        const content = renderContent();
        remember(content);
        const headers: Record<string, string> = {
            'RO-No-Push': 'true',
            'if-none-match': '"spoof-one"',
            'IF-NONE-MATCH': '"spoof-two"',
        };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                target: content,
                headers,
                path: `https://readout.test${TABLE_PATH}#ignored`,
            }),
        );

        expect(headers).toStrictEqual({
            'RO-No-Push': 'true',
            'If-None-Match': ETAG,
        });
    });

    test('accepts the real htmx shape when an exact container request omits target', () => {
        const content = renderContent();
        remember(content);
        const headers = { 'RO-No-Push': 'true' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers).toStrictEqual({
            'RO-No-Push': 'true',
            'If-None-Match': ETAG,
        });
    });

    test('an exact refresh from an emptied current container clears the stored pair and stays unconditional', () => {
        const content = renderContent();
        remember(content);
        expect(content.childElementCount).toBe(1);
        expect(content.dataset.roEtag).toBe(ETAG);
        expect(content.dataset.roEtagPath).toBe(TABLE_PATH);
        content.replaceChildren();
        const headers = { 'RO-No-Push': 'true', 'If-None-Match': '"spoof"' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                target: content,
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });
        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });

    test.each([
        [
            'different query order',
            '/clusters/prod/namespaces/default/pods/_table?f=status%3ARunning&sort=Name',
        ],
        ['different query value', '/clusters/prod/namespaces/default/pods/_table?sort=Age'],
        [
            'different route',
            '/clusters/prod/namespaces/default/services/_table?sort=Name&f=status%3ARunning',
        ],
        ['cross-origin URL', `https://attacker.test${TABLE_PATH}`],
        ['malformed URL', 'https://[/pods/_table'],
        ['non-table URL', '/clusters/prod/namespaces/default/pods'],
        ['missing URL', undefined],
    ])('strips a spoofed conditional for $0', (_name, path) => {
        const content = renderContent();
        remember(content);
        const headers = { 'RO-No-Push': 'true', 'If-None-Match': '"spoof"' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', { elt: content, target: content, headers, path }),
        );

        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });
        expect(content.dataset.roEtag).toBe(ETAG);
    });

    test('keeps user sort/filter requests unconditional, including spoofed headers', () => {
        const content = renderContent();
        remember(content);
        const source = document.createElement('a');
        const headers = { 'If-None-Match': ETAG };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: source,
                target: content,
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers).toStrictEqual({});
    });

    test.each([
        ['no RO-No-Push marker', {}],
        ['wrong RO-No-Push value', { 'RO-No-Push': 'false' }],
    ])('does not attach for $0', (_name, initialHeaders) => {
        const content = renderContent();
        remember(content);
        const headers: Record<string, string> = { ...initialHeaders, 'If-None-Match': '"spoof"' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                target: content,
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers['If-None-Match']).toBeUndefined();
    });

    test('does not attach when the current source explicitly targets another element', () => {
        const content = renderContent();
        remember(content);
        const headers = { 'RO-No-Push': 'true', 'If-None-Match': '"spoof"' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                target: document.createElement('main'),
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });
    });

    test('leaves unrelated request headers alone', () => {
        renderContent();
        const source = document.createElement('button');
        const target = document.createElement('main');
        const headers = { 'If-None-Match': '"belongs-to-another-feature"' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', { elt: source, target, headers, path: '/other' }),
        );

        expect(headers).toStrictEqual({ 'If-None-Match': '"belongs-to-another-feature"' });
    });

    test('clears a corrupt stored pair and never emits either half', () => {
        const content = renderContent();
        content.dataset.roEtag = ETAG;
        content.dataset.roEtagPath = `https://attacker.test${TABLE_PATH}`;
        const headers = { 'RO-No-Push': 'true' };

        configureListValidatorRequest(
            htmxEvent('htmx:configRequest', {
                elt: content,
                target: content,
                headers,
                path: TABLE_PATH,
            }),
        );

        expect(headers).toStrictEqual({ 'RO-No-Push': 'true' });
        expect(content.dataset.roEtag).toBeUndefined();
        expect(content.dataset.roEtagPath).toBeUndefined();
    });
});

describe('exact 304 suppression', () => {
    test('suppresses the exact container conditional with case-insensitive request header names', () => {
        const content = renderContent();
        remember(content);
        const detail = conditionalDetail(content);

        const matched = suppressListNotModified(htmxEvent('htmx:beforeSwap', detail));

        expect(matched).toBe(true);
        expect(detail.shouldSwap).toBe(false);
        expect(detail.isError).toBe(false);
    });

    test('fail-safes every mismatched current-list 304 into a non-swapping error', () => {
        const content = renderContent();
        const detached = document.createElement('div');
        detached.id = 'resource-list-content';
        const cases: Array<[string, (detail: Record<string, unknown>) => void]> = [
            [
                'user source',
                (detail) => {
                    (detail.requestConfig as Record<string, unknown>).elt =
                        document.createElement('a');
                },
            ],
            [
                'detached same-id source',
                (detail) => {
                    (detail.requestConfig as Record<string, unknown>).elt = detached;
                },
            ],
            ['missing request config', (detail) => (detail.requestConfig = undefined)],
            [
                'missing RO-No-Push',
                (detail) =>
                    (detail.requestConfig = {
                        elt: content,
                        headers: { 'If-None-Match': ETAG },
                    }),
            ],
            [
                'wrong RO-No-Push',
                (detail) =>
                    (detail.requestConfig = {
                        elt: content,
                        headers: { 'RO-No-Push': 'false', 'If-None-Match': ETAG },
                    }),
            ],
            [
                'missing If-None-Match',
                (detail) =>
                    (detail.requestConfig = {
                        elt: content,
                        headers: { 'RO-No-Push': 'true' },
                    }),
            ],
            [
                'different If-None-Match',
                (detail) =>
                    (detail.requestConfig = {
                        elt: content,
                        headers: { 'RO-No-Push': 'true', 'If-None-Match': OTHER_ETAG },
                    }),
            ],
            ['missing final path', (detail) => (detail.pathInfo = {})],
            [
                'different final path',
                (detail) => (detail.pathInfo = { finalRequestPath: '/other/_table' }),
            ],
            ['missing response ETag', (detail) => (detail.xhr = responseXHR(304, null))],
            ['different response ETag', (detail) => (detail.xhr = responseXHR(304, OTHER_ETAG))],
            [
                'throwing response-header accessor',
                (detail) =>
                    (detail.xhr = {
                        status: 304,
                        getResponseHeader: () => {
                            throw new Error('broken XHR facade');
                        },
                    }),
            ],
        ];

        for (const [name, mutate] of cases) {
            remember(content);
            const detail = conditionalDetail(content);
            detail.isError = false;
            mutate(detail);

            expect(suppressListNotModified(htmxEvent('htmx:beforeSwap', detail)), name).toBe(false);
            expect(detail.shouldSwap, name).toBe(false);
            expect(detail.isError, name).toBe(true);
        }
    });

    test('a missing or concurrently changed stored validator also fail-safes the 304', () => {
        const content = renderContent();

        for (const state of ['missing', 'changed'] as const) {
            remember(content);
            const detail = conditionalDetail(content);
            detail.isError = false;
            if (state === 'missing') {
                clearListValidator();
            } else {
                content.dataset.roEtag = OTHER_ETAG;
            }

            expect(suppressListNotModified(htmxEvent('htmx:beforeSwap', detail)), state).toBe(
                false,
            );
            expect(detail.shouldSwap, state).toBe(false);
            expect(detail.isError, state).toBe(true);
        }
    });

    test.each([
        [
            'a non-304 response',
            (content: HTMLElement) => ({
                ...conditionalDetail(content),
                xhr: responseXHR(200, ETAG),
            }),
        ],
        [
            'an unrelated target',
            (content: HTMLElement) => ({
                ...conditionalDetail(content),
                target: document.createElement('main'),
            }),
        ],
    ] as const)('leaves %s policy untouched', (_name, makeDetail) => {
        const content = renderContent();
        remember(content);
        const detail: Record<string, unknown> = makeDetail(content);

        expect(suppressListNotModified(htmxEvent('htmx:beforeSwap', detail))).toBe(false);
        expect(detail.shouldSwap).toBe(true);
        expect(detail.isError).toBe(true);
    });

    test('a replaced current container makes the old response inert', () => {
        const oldContent = renderContent();
        remember(oldContent);
        const detail = conditionalDetail(oldContent);
        oldContent.remove();
        const replacement = renderContent();
        replacement.dataset.roEtag = ETAG;
        replacement.dataset.roEtagPath = TABLE_PATH;

        expect(suppressListNotModified(htmxEvent('htmx:beforeSwap', detail))).toBe(false);
        expect(detail.shouldSwap).toBe(true);
        expect(replacement.dataset.roEtag).toBe(ETAG);
    });
});

test('all public handlers are total for malformed lifecycle events and absent DOM', () => {
    const events = [
        new Event('plain'),
        htmxEvent('x', null),
        htmxEvent('x', 0),
        htmxEvent('x', 'detail'),
        htmxEvent('x', {}),
        htmxEvent('x', { headers: null, requestConfig: null, pathInfo: null, xhr: null }),
    ];

    for (const event of events) {
        expect(() => configureListValidatorRequest(event)).not.toThrow();
        expect(() => rememberListValidator(event)).not.toThrow();
        expect(() => suppressListNotModified(event)).not.toThrow();
    }
    expect(() => clearListValidator()).not.toThrow();
});
