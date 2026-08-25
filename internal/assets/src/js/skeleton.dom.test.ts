// @vitest-environment jsdom

import { beforeEach, describe, expect, test, vi } from 'vitest';

const stale = vi.hoisted(() => ({
    isListRefreshEvent: vi.fn((event: Event) => Boolean((event as CustomEvent).detail?.isList)),
}));

vi.mock('./stale.js', () => stale);

import './skeleton.js';

function requestEvent(type: string, isList = true): CustomEvent {
    return new CustomEvent(type, { bubbles: true, detail: { isList } });
}

function renderSkeleton(content = ''): void {
    document.body.innerHTML = `
        <div id="ro-skel-template" hidden>
            <div class="ro-skel">Loading A</div>
            <div class="ro-skel">Loading B</div>
        </div>
        <div id="resource-list-content">${content}</div>
    `;
}

beforeEach(() => {
    renderSkeleton();
});

describe('list skeleton lifecycle', () => {
    test('clones the server template only into an empty list region', () => {
        document.dispatchEvent(requestEvent('htmx:beforeRequest'));

        const content = document.getElementById('resource-list-content');
        expect(content?.querySelectorAll(':scope > .ro-skel')).toHaveLength(2);
        expect(content).toHaveTextContent('Loading A');
        expect(content).toHaveTextContent('Loading B');
    });

    test('never replaces existing rows or diagnostics', () => {
        renderSkeleton('<div class="ro-banner">Permission denied</div>');

        document.dispatchEvent(requestEvent('htmx:beforeRequest'));

        const content = document.getElementById('resource-list-content');
        expect(content).toHaveTextContent('Permission denied');
        expect(content?.querySelector('.ro-skel')).not.toBeInTheDocument();
    });

    test('ignores unrelated requests and missing templates', () => {
        document.dispatchEvent(requestEvent('htmx:beforeRequest', false));
        expect(document.querySelector('#resource-list-content .ro-skel')).not.toBeInTheDocument();

        document.getElementById('ro-skel-template')?.remove();
        document.dispatchEvent(requestEvent('htmx:beforeRequest'));
        expect(document.querySelector('#resource-list-content .ro-skel')).not.toBeInTheDocument();
    });

    test.each(['htmx:responseError', 'htmx:sendError'])(
        '%s removes a stranded skeleton but preserves other content',
        (eventType) => {
            renderSkeleton('<div class="ro-skel">Loading</div><p>Diagnostic</p>');

            document.dispatchEvent(requestEvent(eventType));

            expect(
                document.querySelector('#resource-list-content .ro-skel'),
            ).not.toBeInTheDocument();
            expect(document.getElementById('resource-list-content')).toHaveTextContent(
                'Diagnostic',
            );
        },
    );

    test('error events do not touch a skeleton belonging to another request', () => {
        renderSkeleton('<div class="ro-skel">Loading</div>');

        document.dispatchEvent(requestEvent('htmx:responseError', false));

        expect(document.querySelector('#resource-list-content .ro-skel')).toBeInTheDocument();
    });
});
