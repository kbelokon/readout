// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';

const preferences = vi.hoisted(() => ({
    setNamespace: vi.fn<(cluster: string, namespace: string) => void>(),
}));

vi.mock('./prefs.js', () => ({
    roPrefsSetNamespace: preferences.setNamespace,
}));

import { collapseSectionsFromHash, miscBindings } from './misc-ui.js';

function binding(event: string, selector: string): Binding {
    const found = miscBindings.find(
        (candidate) => candidate.event === event && candidate.selector === selector,
    );
    expect(found).toBeDefined();
    return found as Binding;
}

function clickEvent(): MouseEvent {
    return new MouseEvent('click', { bubbles: true, cancelable: true });
}

function setClipboard(writeText?: (text: string) => Promise<void>): void {
    Object.defineProperty(navigator, 'clipboard', {
        configurable: true,
        value: writeText ? { writeText } : undefined,
    });
}

test('binding descriptors preserve their dispatcher event, selector, and stop contracts', () => {
    expect(miscBindings.map(({ event, selector, stop }) => [event, selector, stop])).toStrictEqual([
        ['click', '[data-ro-action="toggle-sidebar"]', true],
        ['click', '[data-ro-action="copy"]', true],
        ['click', 'main .collapsible h4.title', true],
        ['click', '#namespace-dropdown [data-ro-action="pick-namespace"]', true],
        ['click', '#namespace-dropdown .context-trigger', true],
        ['input', '#namespace-searchbox', true],
        ['keyup', '#namespace-searchbox', true],
        ['click', '[data-ro-more]', true],
        ['click', '[data-ro-annolong]', true],
        ['click', '[data-ro-action="toggle-tools"]', true],
        ['change', 'input[data-ro-toggle-button]', true],
        ['submit', 'form.tools-form', undefined],
    ]);
});

function renderCopySection(): { button: HTMLElement; label: HTMLElement } {
    document.body.innerHTML = `
        <main>
            <section class="collapsible" data-name="yaml">
                <h4 class="title">
                    YAML
                    <button type="button" data-ro-action="copy">
                        <span class="ro-copy-text">copy</span>
                    </button>
                </h4>
                <table class="highlighttable"><tbody><tr><td class="code"><pre><span>apiVersion: v1\n</span><button data-ro-action="toggle-fold">fold</button><span data-ro-fold-control="note"> … 1 line</span><span>kind: Pod\n</span></pre></td></tr></tbody></table>
            </section>
        </main>
    `;
    return {
        button: document.querySelector('[data-ro-action="copy"]') as HTMLElement,
        label: document.querySelector('.ro-copy-text') as HTMLElement,
    };
}

beforeEach(() => {
    document.body.replaceChildren();
    window.history.replaceState(null, '', '/');
    setClipboard();
    vi.stubGlobal('CSS', {
        escape: vi.fn((value: string) => value),
    });
});

describe('sidebar and copy controls', () => {
    test('toggles the mobile sidebar and remains a safe stopped action without one', () => {
        document.body.innerHTML = `
            <button data-ro-action="toggle-sidebar">Menu</button>
            <aside class="ro-sidebar"></aside>
        `;
        const toggle = document.querySelector('[data-ro-action="toggle-sidebar"]') as HTMLElement;
        const sidebar = document.querySelector('.ro-sidebar') as HTMLElement;
        const handler = binding('click', '[data-ro-action="toggle-sidebar"]');
        const openEvent = clickEvent();

        expect(handler.handler(openEvent, toggle)).toBe(true);
        expect(handler.stop).toBe(true);
        expect(openEvent).toHaveProperty('defaultPrevented', true);
        expect(sidebar).toHaveClass('is-active');

        const closeEvent = clickEvent();
        handler.handler(closeEvent, toggle);
        expect(sidebar).not.toHaveClass('is-active');

        sidebar.remove();
        expect(handler.handler(clickEvent(), toggle)).toBe(true);
    });

    test('copies full YAML without injected fold controls and restores the label', async () => {
        vi.useFakeTimers();
        const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
        setClipboard(writeText);
        const { button, label } = renderCopySection();
        const event = clickEvent();

        expect(binding('click', '[data-ro-action="copy"]').handler(event, button)).toBe(true);
        expect(event).toHaveProperty('defaultPrevented', true);
        expect(writeText).toHaveBeenCalledExactlyOnceWith('apiVersion: v1\nkind: Pod\n');

        await Promise.resolve();
        expect(label).toHaveTextContent('copied');
        vi.advanceTimersByTime(1499);
        expect(label).toHaveTextContent('copied');
        vi.advanceTimersByTime(1);
        expect(label).toHaveTextContent('copy');
    });

    test('shows the manual-copy fallback on rejection and cleans it up', async () => {
        vi.useFakeTimers();
        const writeText = vi
            .fn<(text: string) => Promise<void>>()
            .mockRejectedValue(new Error('clipboard denied'));
        setClipboard(writeText);
        const { button, label } = renderCopySection();

        binding('click', '[data-ro-action="copy"]').handler(clickEvent(), button);
        await Promise.resolve();

        expect(label).toHaveTextContent('press ⌘C');
        vi.advanceTimersByTime(1500);
        expect(label).toHaveTextContent('copy');
    });

    test('does not call an available clipboard when the matched control has no YAML', () => {
        vi.useFakeTimers();
        const writeText = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
        setClipboard(writeText);
        document.body.innerHTML = `
            <button data-ro-action="copy"><span class="ro-copy-text">copy</span></button>
        `;
        const button = document.querySelector('[data-ro-action="copy"]') as HTMLElement;

        binding('click', '[data-ro-action="copy"]').handler(clickEvent(), button);

        expect(writeText).not.toHaveBeenCalled();
        expect(button.querySelector('.ro-copy-text')).toHaveTextContent('press ⌘C');
        vi.advanceTimersByTime(1500);
        expect(button.querySelector('.ro-copy-text')).toHaveTextContent('copy');
    });

    test('falls back safely when YAML exists but the Clipboard API is absent', () => {
        vi.useFakeTimers();
        const { button, label } = renderCopySection();
        const event = clickEvent();

        expect(binding('click', '[data-ro-action="copy"]').handler(event, button)).toBe(true);
        expect(event.defaultPrevented).toBe(true);
        expect(label).toHaveTextContent('press ⌘C');

        vi.advanceTimersByTime(1500);
        expect(label).toHaveTextContent('copy');
    });
});

describe('section collapse and hash state', () => {
    test('restores every matching section from the collapsed hash', () => {
        document.body.innerHTML = `
            <main>
                <section class="collapsible" data-name="alpha"></section>
                <section class="collapsible" data-name="beta"></section>
                <section class="collapsible" data-name="alpha"></section>
            </main>
        `;
        window.history.replaceState(null, '', '/objects#other=x;collapsed=alpha,beta');

        collapseSectionsFromHash();

        expect(document.querySelectorAll('[data-name="alpha"].is-collapsed')).toHaveLength(2);
        expect(document.querySelector('[data-name="beta"]')).toHaveClass('is-collapsed');
        expect(CSS.escape).toHaveBeenCalledWith('alpha');
        expect(CSS.escape).toHaveBeenCalledWith('beta');
    });

    test('folds a title nested in a card head and removes an empty hash without losing query', () => {
        document.body.innerHTML = `
            <main>
                <section class="collapsible" data-name="alpha">
                    <div class="ro-card-head"><h4 class="title">Alpha</h4></div>
                </section>
                <section class="collapsible is-collapsed" data-name="beta">
                    <h4 class="title">Beta</h4>
                </section>
            </main>
        `;
        window.history.replaceState(null, '', '/objects?kind=pods');
        const titles = document.querySelectorAll('h4.title');
        const fold = binding('click', 'main .collapsible h4.title');
        const foldEvent = clickEvent();

        expect(fold.handler(foldEvent, titles[0])).toBe(true);
        expect(foldEvent.defaultPrevented).toBe(false);
        expect(titles[0].closest('.collapsible')).toHaveClass('is-collapsed');
        expect(window.location.hash).toBe('#collapsed=alpha,beta');

        fold.handler(clickEvent(), titles[0]);
        expect(window.location.hash).toBe('#collapsed=beta');

        fold.handler(clickEvent(), titles[1]);
        expect(window.location.href).toBe('https://readout.test/objects?kind=pods');
    });

    test('a detached title remains a safe stopped no-op', () => {
        document.body.innerHTML = '<h4 class="title">Detached</h4>';
        window.history.replaceState(null, '', '/objects#collapsed=alpha');
        const title = document.querySelector('h4.title') as HTMLElement;

        expect(binding('click', 'main .collapsible h4.title').handler(clickEvent(), title)).toBe(
            true,
        );
        expect(window.location.hash).toBe('#collapsed=alpha');
    });
});

describe('namespace dropdown', () => {
    test('persists decoded cluster and namespace names only for a valid namespace href', () => {
        document.body.innerHTML = `
            <div id="namespace-dropdown">
                <a id="valid" data-ro-action="pick-namespace"
                    href="/clusters/prod%20east/namespaces/team%2Fblue/pods">Team</a>
                <a id="invalid" data-ro-action="pick-namespace" href="/clusters">Invalid</a>
                <a id="embedded" data-ro-action="pick-namespace"
                    href="/prefix/clusters/prod/namespaces/team/pods">Embedded</a>
                <a id="missing" data-ro-action="pick-namespace">Missing</a>
            </div>
        `;
        const pick = binding('click', '#namespace-dropdown [data-ro-action="pick-namespace"]');
        const valid = document.getElementById('valid') as HTMLAnchorElement;
        const event = clickEvent();

        expect(pick.handler(event, valid)).toBe(true);
        expect(event).toHaveProperty('defaultPrevented', false);
        expect(preferences.setNamespace).toHaveBeenCalledExactlyOnceWith('prod east', 'team/blue');

        pick.handler(clickEvent(), document.getElementById('invalid'));
        pick.handler(clickEvent(), document.getElementById('embedded'));
        pick.handler(clickEvent(), document.getElementById('missing'));
        expect(preferences.setNamespace).toHaveBeenCalledOnce();
    });

    test('opens and closes the dropdown and focuses search only when opening', () => {
        document.body.innerHTML = `
            <div id="namespace-dropdown">
                <button class="context-trigger">Namespaces</button>
                <input id="namespace-searchbox">
            </div>
        `;
        const dropdown = document.getElementById('namespace-dropdown') as HTMLElement;
        const trigger = dropdown.querySelector('.context-trigger') as HTMLElement;
        const search = document.getElementById('namespace-searchbox') as HTMLInputElement;
        const toggle = binding('click', '#namespace-dropdown .context-trigger');
        const focus = vi.spyOn(search, 'focus');

        expect(toggle.handler(clickEvent(), trigger)).toBe(true);
        expect(dropdown).toHaveClass('is-active');
        expect(document.activeElement).toBe(search);
        expect(focus).toHaveBeenCalledOnce();

        expect(toggle.handler(clickEvent(), trigger)).toBe(true);
        expect(dropdown).not.toHaveClass('is-active');
        expect(focus).toHaveBeenCalledOnce();
    });

    test('a detached dropdown trigger remains a safe stopped no-op', () => {
        document.body.innerHTML = '<button class="context-trigger">Namespaces</button>';
        const trigger = document.querySelector('.context-trigger') as HTMLElement;

        expect(
            binding('click', '#namespace-dropdown .context-trigger').handler(clickEvent(), trigger),
        ).toBe(true);
    });

    test('filters case-insensitively and Enter clicks the first visible namespace', () => {
        document.body.innerHTML = `
            <div id="namespace-dropdown">
                <input id="namespace-searchbox">
                <a data-ro-action="pick-namespace">Production</a>
                <a data-ro-action="pick-namespace">Development</a>
                <a data-ro-action="pick-namespace">Developer Tools</a>
            </div>
        `;
        const search = document.getElementById('namespace-searchbox') as HTMLInputElement;
        const links = Array.from(
            document.querySelectorAll('[data-ro-action="pick-namespace"]'),
        ) as HTMLElement[];
        links[0].innerText = 'Production';
        links[1].innerText = 'Development';
        links[2].innerText = 'Developer Tools';
        const clicks = links.map((link) => vi.spyOn(link, 'click').mockImplementation(() => {}));
        const filter = binding('input', '#namespace-searchbox');
        search.value = 'prod';

        expect(filter.handler(new InputEvent('input'), search)).toBe(true);

        expect(links[0]).not.toHaveClass('is-hidden');
        expect(links[1]).toHaveClass('is-hidden');
        expect(links[2]).toHaveClass('is-hidden');

        search.value = 'DEV';
        expect(filter.handler(new InputEvent('input'), search)).toBe(true);

        expect(links[0]).toHaveClass('is-hidden');
        expect(links[1]).not.toHaveClass('is-hidden');
        expect(links[2]).not.toHaveClass('is-hidden');

        const enter = binding('keyup', '#namespace-searchbox');
        expect(enter.handler(new KeyboardEvent('keyup', { key: 'Escape' }), search)).toBe(true);
        expect(clicks.every((click) => click.mock.calls.length === 0)).toBe(true);

        expect(enter.handler(new KeyboardEvent('keyup', { key: 'Enter' }), search)).toBe(true);
        expect(clicks[0]).not.toHaveBeenCalled();
        expect(clicks[1]).toHaveBeenCalledOnce();
        expect(clicks[2]).not.toHaveBeenCalled();

        clicks.forEach((click) => {
            click.mockClear();
        });
        links.forEach((link) => {
            link.classList.add('is-hidden');
        });
        expect(enter.handler(new KeyboardEvent('keyup', { key: 'Enter' }), search)).toBe(true);
        expect(clicks.every((click) => click.mock.calls.length === 0)).toBe(true);

        links.forEach((link) => {
            link.remove();
        });
        expect(enter.handler(new KeyboardEvent('keyup', { key: 'Enter' }), search)).toBe(true);
    });
});

describe('transient detail controls', () => {
    test('toggles only the owning overflow chip strip and mirrors aria-expanded', () => {
        document.body.innerHTML = `
            <div class="ro-chips" id="first"><button data-ro-more aria-expanded="false">+2</button></div>
            <div class="ro-chips" id="second"></div>
        `;
        const chips = document.getElementById('first') as HTMLElement;
        const button = chips.querySelector('[data-ro-more]') as HTMLElement;
        const more = binding('click', '[data-ro-more]');
        const openEvent = clickEvent();

        expect(more.handler(openEvent, button)).toBe(true);
        expect(openEvent).toHaveProperty('defaultPrevented', true);
        expect(chips).toHaveClass('expanded');
        expect(document.getElementById('second')).not.toHaveClass('expanded');
        expect(button).toHaveAttribute('aria-expanded', 'true');

        const closeEvent = clickEvent();
        expect(more.handler(closeEvent, button)).toBe(true);
        expect(closeEvent.defaultPrevented).toBe(true);
        expect(chips).not.toHaveClass('expanded');
        expect(button).toHaveAttribute('aria-expanded', 'false');
    });

    test('opens and closes a long annotation payload with matching visual state', () => {
        document.body.innerHTML = `
            <div class="annotation">
                <button data-ro-annolong aria-expanded="false">key</button>
                <pre class="anno-pre" hidden>long value</pre>
            </div>
        `;
        const button = document.querySelector('[data-ro-annolong]') as HTMLElement;
        const pre = document.querySelector('.anno-pre') as HTMLElement;
        const annotation = binding('click', '[data-ro-annolong]');
        const openEvent = clickEvent();

        expect(annotation.handler(openEvent, button)).toBe(true);
        expect(openEvent.defaultPrevented).toBe(true);
        expect(pre).not.toHaveAttribute('hidden');
        expect(button).toHaveClass('open');
        expect(button).toHaveAttribute('aria-expanded', 'true');

        const closeEvent = clickEvent();
        expect(annotation.handler(closeEvent, button)).toBe(true);
        expect(closeEvent.defaultPrevented).toBe(true);
        expect(pre).toHaveAttribute('hidden');
        expect(button).not.toHaveClass('open');
        expect(button).toHaveAttribute('aria-expanded', 'false');
    });

    test('toggles the tools control and its named target together', () => {
        document.body.innerHTML = `
            <button data-ro-action="toggle-tools" data-target="tools-panel">Tools</button>
            <section id="tools-panel"></section>
        `;
        const button = document.querySelector('[data-ro-action="toggle-tools"]') as HTMLElement;
        const panel = document.getElementById('tools-panel') as HTMLElement;
        const tools = binding('click', '[data-ro-action="toggle-tools"]');
        const openEvent = clickEvent();

        expect(tools.handler(openEvent, button)).toBe(true);
        expect(openEvent.defaultPrevented).toBe(true);
        expect(button).toHaveClass('is-active');
        expect(panel).toHaveClass('is-active');

        const closeEvent = clickEvent();
        expect(tools.handler(closeEvent, button)).toBe(true);
        expect(closeEvent.defaultPrevented).toBe(true);
        expect(button).not.toHaveClass('is-active');
        expect(panel).not.toHaveClass('is-active');
    });
});

describe('form glue', () => {
    test('enables a named button while any checkbox in its group is checked', () => {
        document.body.innerHTML = `
            <input id="one" type="checkbox" data-ro-toggle-button="search-button">
            <input id="two" type="checkbox" data-ro-toggle-button="search-button">
            <input type="checkbox" data-ro-toggle-button="other-button" checked>
            <button id="search-button" disabled>Search</button>
        `;
        const one = document.getElementById('one') as HTMLInputElement;
        const two = document.getElementById('two') as HTMLInputElement;
        const button = document.getElementById('search-button') as HTMLButtonElement;
        const change = binding('change', 'input[data-ro-toggle-button]');

        one.checked = true;
        expect(change.handler(new Event('change'), one)).toBe(true);
        expect(button).toBeEnabled();

        two.checked = true;
        one.checked = false;
        expect(change.handler(new Event('change'), one)).toBe(true);
        expect(button).toBeEnabled();

        two.checked = false;
        expect(change.handler(new Event('change'), two)).toBe(true);
        expect(button).toBeDisabled();
    });

    test('a checkbox naming an absent button remains a safe stopped no-op', () => {
        document.body.innerHTML = `
            <input type="checkbox" data-ro-toggle-button="missing-button" checked>
        `;
        const checkbox = document.querySelector('input') as HTMLInputElement;

        expect(
            binding('change', 'input[data-ro-toggle-button]').handler(
                new Event('change'),
                checkbox,
            ),
        ).toBe(true);
    });

    test('removes names only from empty inputs and leaves the GET submit unblocked', () => {
        document.body.innerHTML = `
            <form class="tools-form">
                <input id="empty" name="empty" value="">
                <input id="filled" name="filled" value="0">
                <input id="unnamed" value="">
            </form>
        `;
        const form = document.querySelector('form.tools-form') as HTMLFormElement;
        const submit = binding('submit', 'form.tools-form');
        const event = new SubmitEvent('submit', { cancelable: true });

        expect(submit.handler(event, form)).toBeUndefined();
        expect(event.defaultPrevented).toBe(false);
        expect(submit.stop).toBeUndefined();
        expect(document.getElementById('empty')).toHaveAttribute('name', '');
        expect(document.getElementById('filled')).toHaveAttribute('name', 'filled');
        expect(document.getElementById('unnamed')).not.toHaveAttribute('name');
    });
});
