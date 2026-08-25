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
                <td class="code"><pre><span id="yaml-doc-line-1"><a></a>spec:\n</span><span id="yaml-doc-line-2"><a></a>  template:\n</span><span id="yaml-doc-line-3"><a></a>    metadata:\n</span><span id="yaml-doc-line-4"><a></a>      name: api\n</span><span id="yaml-doc-line-5"><a></a>status: ready\n</span></pre></td>
            </tr></tbody>
        </table>
    `;
    return document.querySelector('td.code') as HTMLElement;
}

function binding(selector: string): Binding {
    const found = foldBindings.find((item) => item.selector === selector);
    expect(found).toBeDefined();
    return found as Binding;
}

describe('YAML fold builder', () => {
    beforeEach(() => {
        renderYaml();
    });

    test('builds nested ownership and accurate fold counts', () => {
        buildYamlFolds();

        const toggles = document.querySelectorAll('[data-ro-action="toggle-fold"]');
        expect(toggles).toHaveLength(3);
        expect(toggles[0]).toHaveAttribute('data-fold', 'yaml-doc-line-1');
        expect(toggles[0].parentElement).toHaveTextContent('… 3 lines');
        expect(toggles[1].parentElement).toHaveTextContent('… 2 lines');
        expect(toggles[2].parentElement).toHaveTextContent('… 1 line');
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
        expect(() => highlightYamlLine()).not.toThrow();
        window.history.replaceState(null, '', '/resource#missing-line');
        expect(() => highlightYamlLine()).not.toThrow();
        expect(document.querySelector('.yaml-line-highlight')).not.toBeInTheDocument();
    });
});
