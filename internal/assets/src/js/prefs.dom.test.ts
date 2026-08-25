// @vitest-environment jsdom

import { beforeEach, expect, test, vi } from 'vitest';

import {
    PREFS_COOKIE,
    PREFS_COOKIE_MAX_AGE,
    type Prefs,
    readPrefs,
    roPrefsSetHiddenColumns,
    roPrefsSetNamespace,
    roPrefsSetRefresh,
    roPrefsSetSort,
    writePrefs,
} from './prefs.js';

const emptyPrefs: Prefs = { kinds: [], refresh: '', ns: {} };

function clearPrefsCookie(): void {
    document.cookie = `${PREFS_COOKIE}=; Path=/; Max-Age=0`;
}

beforeEach(() => {
    clearPrefsCookie();
    document.cookie = 'unrelated=; Path=/; Max-Age=0';
});

test('readPrefs returns empty prefs for an absent or corrupt cookie', () => {
    expect(readPrefs()).toStrictEqual(emptyPrefs);

    document.cookie = `${PREFS_COOKIE}=v1.%%%; Path=/`;
    expect(readPrefs()).toStrictEqual(emptyPrefs);
});

test('writePrefs round-trips through document.cookie without replacing unrelated cookies', () => {
    const prefs: Prefs = {
        kinds: [{ k: 'pods', sort: 'Status:desc', hide: ['Node', 'Age'] }],
        refresh: '30',
        ns: { test: 'default' },
    };
    document.cookie = 'unrelated=kept; Path=/';

    writePrefs(prefs);

    expect(readPrefs()).toStrictEqual(prefs);
    expect(document.cookie).toContain('unrelated=kept');
});

test('writePrefs assigns the persistent cookie attributes for the current protocol', () => {
    const cookieSetter = vi.spyOn(document, 'cookie', 'set');

    writePrefs({ kinds: [], refresh: 'Live', ns: {} });

    expect(cookieSetter).toHaveBeenCalledOnce();
    const assigned = cookieSetter.mock.calls[0][0];
    expect(assigned).toContain(`${PREFS_COOKIE}=v1.`);
    expect(assigned).toContain('; Path=/; SameSite=Lax;');
    expect(assigned).toContain(`Max-Age=${PREFS_COOKIE_MAX_AGE}`);
    if (window.location.protocol === 'https:') {
        expect(assigned).toContain('; Secure');
    } else {
        expect(assigned).not.toContain('; Secure');
    }
    expect(readPrefs().refresh).toBe('Live');
});

test('roPrefsSetSort preserves other fields and moves the touched kind to the front', () => {
    writePrefs({
        kinds: [
            { k: 'deployments', sort: 'Age', hide: ['Ready'] },
            { k: 'pods', sort: 'Name', hide: ['Node'] },
            { k: 'services', hide: ['Cluster IP'] },
        ],
        refresh: '10',
        ns: { test: 'kube-system' },
    });

    roPrefsSetSort('pods', 'Status:desc');

    expect(readPrefs()).toStrictEqual({
        kinds: [
            { k: 'pods', sort: 'Status:desc', hide: ['Node'] },
            { k: 'deployments', sort: 'Age', hide: ['Ready'] },
            { k: 'services', hide: ['Cluster IP'] },
        ],
        refresh: '10',
        ns: { test: 'kube-system' },
    });
});

test('roPrefsSetHiddenColumns persists an explicit empty hidden set', () => {
    writePrefs({
        kinds: [{ k: 'pods', sort: 'Name', hide: ['Node'] }],
        refresh: '',
        ns: {},
    });

    roPrefsSetHiddenColumns('pods', []);

    expect(readPrefs()).toStrictEqual({
        kinds: [{ k: 'pods', sort: 'Name', hide: [] }],
        refresh: '',
        ns: {},
    });
});

test('roPrefsSetRefresh changes only the refresh preference', () => {
    const original: Prefs = {
        kinds: [{ k: 'pods', sort: 'Name' }],
        refresh: '5',
        ns: { test: 'default' },
    };
    writePrefs(original);

    roPrefsSetRefresh('Live');

    expect(readPrefs()).toStrictEqual({ ...original, refresh: 'Live' });
});

test('roPrefsSetNamespace records a namespace while preserving existing preferences', () => {
    writePrefs({
        kinds: [{ k: 'pods', hide: ['Node'] }],
        refresh: '30',
        ns: { other: 'default' },
    });

    roPrefsSetNamespace('test', '_all');

    expect(readPrefs()).toStrictEqual({
        kinds: [{ k: 'pods', hide: ['Node'] }],
        refresh: '30',
        ns: { other: 'default', test: '_all' },
    });
});

test('roPrefsSetNamespace ignores an empty cluster or namespace without writing', () => {
    writePrefs({
        kinds: [{ k: 'pods', sort: 'Name' }],
        refresh: '10',
        ns: { test: 'default' },
    });
    const before = document.cookie;
    const cookieSetter = vi.spyOn(document, 'cookie', 'set');

    roPrefsSetNamespace('', 'default');
    roPrefsSetNamespace('test', '');

    expect(cookieSetter).not.toHaveBeenCalled();
    expect(document.cookie).toBe(before);
});
