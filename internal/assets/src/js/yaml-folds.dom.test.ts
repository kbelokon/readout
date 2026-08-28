// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

import type { Binding } from './events.js';
import { buildYamlFolds, foldBindings, highlightYamlLine, yamlCodeText } from './yaml-folds.js';

const RAW_YAML = 'spec:\n  template:\n    metadata:\n      name: api\nstatus: ready\n';

function renderYaml(): HTMLElement {
    document.body.innerHTML = `
        <table class="highlighttable">
            <tbody><tr>
                <td class="linenos"><a href="#doc-line-4">4</a></td>
                <td class="code"><pre><span id="yaml-doc-line-1"><a></a><span>spec:</span>\n</span><span id="yaml-doc-line-2"><a></a><span>  template:</span>\n</span><span id="yaml-doc-line-3"><a></a><span>    metadata:</span>\n</span><span id="yaml-doc-line-4"><a></a><span>      name: api</span>\n</span><span id="yaml-doc-line-5"><a></a><span>status: ready</span>\n</span></pre></td>
            </tr></tbody>
        </table>
    `;
    return document.querySelector('td.code') as HTMLElement;
}

function binding(selector: string, bindings: readonly Binding[] = foldBindings): Binding {
    const found = bindings.find((item) => item.selector === selector);
    expect(found).toBeDefined();
    return found as Binding;
}

function expectFoldBindingContract(bindings: readonly Binding[]): void {
    expect(bindings.map(({ event, selector, stop }) => ({ event, selector, stop }))).toStrictEqual([
        { event: 'click', selector: '[data-ro-action="toggle-fold"]', stop: true },
        { event: 'click', selector: '.linenos a', stop: true },
    ]);
}

test('fold binding descriptors preserve their dispatcher contracts', () => {
    expectFoldBindingContract(foldBindings);
});

test('freshly loaded bindings drive fold and gutter behavior', async () => {
    vi.resetModules();
    const fresh = await import('./yaml-folds.js');
    const code = renderYaml();
    expectFoldBindingContract(fresh.foldBindings);

    fresh.buildYamlFolds();
    const toggle = code.querySelector('[data-fold="yaml-doc-line-2"]') as HTMLButtonElement;
    const foldEvent = new MouseEvent('click', { bubbles: true, cancelable: true });

    expect(
        binding('[data-ro-action="toggle-fold"]', fresh.foldBindings).handler(foldEvent, toggle),
    ).toBe(true);
    expect(foldEvent.defaultPrevented).toBe(true);
    expect(foldEvent.cancelBubble).toBe(true);
    expect(document.getElementById('yaml-doc-line-3')).toHaveClass('ro-line-folded');
    expect(document.getElementById('yaml-doc-line-4')).toHaveClass('ro-line-folded');

    const target = document.getElementById('yaml-doc-line-4') as HTMLElement;
    target.scrollIntoView = vi.fn();
    const anchor = document.querySelector('.linenos a') as HTMLAnchorElement;
    const gutterEvent = new MouseEvent('click', { bubbles: true, cancelable: true });

    expect(binding('.linenos a', fresh.foldBindings).handler(gutterEvent, anchor)).toBe(true);
    expect(window.location.hash).toBe('#doc-line-4');
    expect(target).toHaveClass('yaml-line-highlight');
    expect(target.scrollIntoView).toHaveBeenCalledExactlyOnceWith({ block: 'center' });
    expect(gutterEvent.defaultPrevented).toBe(true);
});

describe('YAML fold builder', () => {
    beforeEach(() => {
        renderYaml();
    });

    test('builds nested ownership and accurate fold counts', () => {
        buildYamlFolds();

        const toggles = document.querySelectorAll('[data-ro-action="toggle-fold"]');
        expect(toggles).toHaveLength(3);
        expect(toggles[0]).toHaveAttribute('type', 'button');
        expect(toggles[0]).toHaveClass('ro-fold-toggle');
        expect(toggles[0]).toHaveAttribute('data-fold', 'yaml-doc-line-1');
        expect(toggles[0]).toHaveAttribute('aria-expanded', 'true');
        expect(toggles[0]).toHaveAttribute('aria-label', 'Toggle block');

        const notes = document.querySelectorAll('[data-ro-fold-control="note"]');
        expect(notes).toHaveLength(3);
        expect(notes[0]).toHaveClass('ro-fold-note');
        expect(notes[0].textContent).toBe(' … 3 lines');
        expect(notes[1].textContent).toBe(' … 2 lines');
        expect(notes[2].textContent).toBe(' … 1 line');
        expect(notes[0].nextSibling).toBeInstanceOf(Text);
        expect((notes[0].nextSibling as Text).data).toBe('\n');
        expect(document.getElementById('yaml-doc-line-4')).toHaveAttribute(
            'data-fold-of',
            'yaml-doc-line-1 yaml-doc-line-2 yaml-doc-line-3',
        );
    });

    test('is idempotent across repeated init passes', () => {
        buildYamlFolds();
        buildYamlFolds();

        expect(document.querySelectorAll('[data-ro-action="toggle-fold"]')).toHaveLength(3);
        expect(document.querySelector('pre')).toHaveAttribute('data-ro-folds', '1');
    });

    test('fold gesture hides exactly the owned descendants and toggles aria', () => {
        buildYamlFolds();
        const toggle = document.querySelector('[data-fold="yaml-doc-line-2"]') as HTMLButtonElement;
        const event = new MouseEvent('click', { bubbles: true, cancelable: true });

        expect(binding('[data-ro-action="toggle-fold"]').handler(event, toggle)).toBe(true);

        expect(event.defaultPrevented).toBe(true);
        expect(event.cancelBubble).toBe(true);
        expect(toggle).toHaveClass('is-folded');
        expect(toggle).toHaveAttribute('aria-expanded', 'false');
        expect(document.getElementById('yaml-doc-line-2')).not.toHaveClass('ro-line-folded');
        expect(document.getElementById('yaml-doc-line-3')).toHaveClass('ro-line-folded');
        expect(document.getElementById('yaml-doc-line-4')).toHaveClass('ro-line-folded');
        expect(document.getElementById('yaml-doc-line-5')).not.toHaveClass('ro-line-folded');

        binding('[data-ro-action="toggle-fold"]').handler(
            new MouseEvent('click', { cancelable: true }),
            toggle,
        );
        expect(toggle).not.toHaveClass('is-folded');
        expect(toggle).toHaveAttribute('aria-expanded', 'true');
        expect(document.getElementById('yaml-doc-line-3')).not.toHaveClass('ro-line-folded');
    });

    test('copied YAML excludes injected controls in every fold state', () => {
        const code = document.querySelector('td.code') as HTMLElement;
        expect(yamlCodeText(code)).toBe(RAW_YAML);

        buildYamlFolds();
        const toggle = document.querySelector('[data-fold="yaml-doc-line-1"]') as HTMLButtonElement;
        binding('[data-ro-action="toggle-fold"]').handler(
            new MouseEvent('click', { cancelable: true }),
            toggle,
        );

        expect(yamlCodeText(code)).toBe(RAW_YAML);
        expect(yamlCodeText(code)).not.toContain('Toggle block');
        expect(yamlCodeText(code)).not.toContain('…');
    });

    test('reads a large control-free code cell without cloning or cleaning a copy', () => {
        const code = document.createElement('div');
        const expected = Array.from(
            { length: 2_048 },
            (_, index) => `key-${index}: value-${index}\n`,
        );
        const fragment = document.createDocumentFragment();
        expected.forEach((text) => {
            const line = document.createElement('span');
            line.textContent = text;
            fragment.append(line);
        });
        code.append(fragment);
        const cloneNode = vi.spyOn(code, 'cloneNode');
        const querySelectorAll = vi.spyOn(code, 'querySelectorAll');

        expect(yamlCodeText(code)).toBe(expected.join(''));
        expect(cloneNode).not.toHaveBeenCalled();
        expect(querySelectorAll).not.toHaveBeenCalled();
    });

    test('marks a short block processed without injecting meaningless controls', () => {
        document.body.innerHTML = `
            <table class="highlighttable"><tbody><tr><td class="code"><pre>
                <span id="yaml-short-line-1">key:\n</span>
                <span id="yaml-short-line-2">  value\n</span>
            </pre></td></tr></tbody></table>
        `;

        buildYamlFolds();

        expect(document.querySelector('pre')).toHaveAttribute('data-ro-folds', '1');
        expect(document.querySelector('[data-ro-action="toggle-fold"]')).not.toBeInTheDocument();
    });

    test('three-line blocks fold with no anchor or with an anchor-only opener', () => {
        document.body.innerHTML = `
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-no-anchor-line-1"><span>root:</span>\n</span><span id="yaml-no-anchor-line-2">  child:\n</span><span id="yaml-no-anchor-line-3">    leaf: yes\n</span></pre></td></tr></tbody></table>
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-anchor-line-1"><a>root:</a></span><span id="yaml-anchor-line-2">  child: yes\n</span><span id="yaml-anchor-line-3">done: yes\n</span></pre></td></tr></tbody></table>
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-text-line-1"><a></a>root:</span><span id="yaml-text-line-2">  child: yes\n</span><span id="yaml-text-line-3">done: yes\n</span></pre></td></tr></tbody></table>
        `;

        buildYamlFolds();

        expect(document.querySelectorAll('[data-ro-action="toggle-fold"]')).toHaveLength(4);

        const noAnchorOpener = document.getElementById('yaml-no-anchor-line-1') as HTMLElement;
        const noAnchorToggle = noAnchorOpener.querySelector('button') as HTMLButtonElement;
        const noAnchorNote = noAnchorOpener.querySelector('.ro-fold-note') as HTMLElement;
        expect(noAnchorOpener.firstElementChild).toBe(noAnchorToggle);
        expect(noAnchorNote.textContent).toBe(' … 2 lines');
        expect(noAnchorNote.nextSibling).toBeInstanceOf(Text);
        expect((noAnchorNote.nextSibling as Text).data).toBe('\n');

        const anchorOpener = document.getElementById('yaml-anchor-line-1') as HTMLElement;
        const anchorToggle = anchorOpener.querySelector('button') as HTMLButtonElement;
        const anchorNote = anchorOpener.querySelector('.ro-fold-note') as HTMLElement;
        expect(anchorToggle.previousElementSibling?.tagName).toBe('A');
        expect(anchorNote).toBe(anchorOpener.lastChild);
        expect(anchorNote.textContent).toBe(' … 1 line');

        const textOpener = document.getElementById('yaml-text-line-1') as HTMLElement;
        const textNote = textOpener.querySelector('.ro-fold-note') as HTMLElement;
        expect(textNote).toBe(textOpener.lastChild);
        expect(textNote.previousSibling).toBeInstanceOf(Text);
        expect((textNote.previousSibling as Text).data).toBe('root:');
    });

    test('blank lines neither end a block nor count as folded children', () => {
        document.body.innerHTML = `
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-blank-line-1">root:\n</span><span id="yaml-blank-line-2">  \n</span><span id="yaml-blank-line-3">    child: yes\n</span><span id="yaml-blank-line-4">\n</span><span id="yaml-blank-line-5">sibling: yes\n</span></pre></td></tr></tbody></table>
        `;

        buildYamlFolds();

        expect(document.querySelectorAll('[data-ro-action="toggle-fold"]')).toHaveLength(1);
        expect(document.querySelector('.ro-fold-note')?.textContent).toBe(' … 1 line');
        expect(document.getElementById('yaml-blank-line-3')).toHaveAttribute(
            'data-fold-of',
            'yaml-blank-line-1',
        );
        expect(document.getElementById('yaml-blank-line-2')).not.toHaveAttribute('data-fold-of');
        expect(document.getElementById('yaml-blank-line-4')).not.toHaveAttribute('data-fold-of');
        expect(document.getElementById('yaml-blank-line-5')).not.toHaveAttribute('data-fold-of');
    });

    test('a blank tail has no next line and an equal-indent neighbor is not a body', () => {
        document.body.innerHTML = `
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-tail-line-1">root:\n</span><span id="yaml-tail-line-2">  child: yes\n</span><span id="yaml-tail-line-3">  \n</span></pre></td></tr></tbody></table>
            <table class="highlighttable"><tbody><tr><td class="code"><pre><span id="yaml-equal-line-1">first: yes\n</span><span id="yaml-equal-line-2">second: yes\n</span><span id="yaml-equal-line-3">third: yes\n</span></pre></td></tr></tbody></table>
        `;

        buildYamlFolds();

        expect(document.querySelectorAll('[data-ro-action="toggle-fold"]')).toHaveLength(1);
        expect(document.getElementById('yaml-tail-line-2')?.querySelector('button')).toBeNull();
        expect(document.getElementById('yaml-tail-line-3')?.querySelector('button')).toBeNull();
        expect(document.getElementById('yaml-equal-line-1')?.querySelector('button')).toBeNull();
    });
});

describe('YAML line anchors', () => {
    beforeEach(() => {
        renderYaml();
    });

    test('replaces an old highlight and scrolls the requested line to center', () => {
        document.getElementById('yaml-doc-line-1')?.classList.add('yaml-line-highlight');
        const target = document.getElementById('yaml-doc-line-4') as HTMLElement;
        const scrollIntoView = vi.fn();
        target.scrollIntoView = scrollIntoView;
        window.history.replaceState(null, '', '/resource#doc-line-4');

        highlightYamlLine();

        expect(document.getElementById('yaml-doc-line-1')).not.toHaveClass('yaml-line-highlight');
        expect(target).toHaveClass('yaml-line-highlight');
        expect(scrollIntoView).toHaveBeenCalledExactlyOnceWith({ block: 'center' });
    });

    test('gutter binding updates the hash, highlights, and prevents the native jump', () => {
        const target = document.getElementById('yaml-doc-line-4') as HTMLElement;
        target.scrollIntoView = vi.fn();
        const anchor = document.querySelector('.linenos a') as HTMLAnchorElement;
        const event = new MouseEvent('click', { bubbles: true, cancelable: true });

        expect(binding('.linenos a').handler(event, anchor)).toBe(true);

        expect(window.location.hash).toBe('#doc-line-4');
        expect(target).toHaveClass('yaml-line-highlight');
        expect(event.defaultPrevented).toBe(true);
    });

    test('does nothing for an empty or unknown fragment', () => {
        window.history.replaceState(null, '', '/resource');
        const current = document.getElementById('yaml-doc-line-1') as HTMLElement;
        current.classList.add('yaml-line-highlight');

        expect(() => highlightYamlLine()).not.toThrow();
        expect(current).toHaveClass('yaml-line-highlight');

        window.history.replaceState(null, '', '/resource#missing-line');
        expect(() => highlightYamlLine()).not.toThrow();
        expect(document.querySelector('.yaml-line-highlight')).not.toBeInTheDocument();
    });
});
