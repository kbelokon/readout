/** @vitest-environment jsdom */

import { expect, test, vi } from 'vitest';

import { type Binding, registerBindings } from './events.js';

function dispatch(target: EventTarget, type: string): void {
    target.dispatchEvent(new Event(type, { bubbles: true }));
}

test('runs every matching binding in registration order', () => {
    const eventType = 'readout:test:events:ordered';
    const shell = document.createElement('div');
    shell.dataset.shell = 'true';
    const button = document.createElement('button');
    button.dataset.action = 'open';
    const target = document.createElement('span');
    button.appendChild(target);
    shell.appendChild(button);
    document.body.appendChild(shell);

    const calls: string[] = [];
    const bindings: Binding[] = [
        {
            event: eventType,
            selector: '[data-action="open"]',
            handler: (_event, matched) => {
                calls.push('action:first');
                expect(matched).toBe(button);
            },
        },
        {
            event: eventType,
            selector: '[data-shell]',
            handler: (_event, matched) => {
                calls.push('shell');
                expect(matched).toBe(shell);
            },
        },
        {
            event: eventType,
            handler: (_event, matched) => {
                calls.push('unscoped');
                expect(matched).toBeNull();
            },
        },
        {
            event: eventType,
            selector: '[data-action="open"]',
            handler: () => {
                calls.push('action:last');
            },
        },
    ];

    registerBindings(bindings);
    dispatch(target, eventType);

    expect(calls).toStrictEqual(['action:first', 'shell', 'unscoped', 'action:last']);
});

test('skips a selector miss without affecting later bindings', () => {
    const eventType = 'readout:test:events:selector-miss';
    const target = document.createElement('button');
    target.dataset.hit = 'true';
    document.body.appendChild(target);
    const missed = vi.fn();
    const matched = vi.fn();

    registerBindings([
        { event: eventType, selector: '[data-missing]', handler: missed },
        { event: eventType, selector: '[data-hit]', handler: matched },
    ]);
    dispatch(target, eventType);

    expect(missed).not.toHaveBeenCalled();
    expect(matched).toHaveBeenCalledOnce();
    expect(matched).toHaveBeenCalledWith(expect.any(Event), target);
});

test('stops only when stop is true and the handler returns true', () => {
    const eventType = 'readout:test:events:explicit-stop';
    const calls: string[] = [];

    registerBindings([
        {
            event: eventType,
            stop: true,
            handler: () => {
                calls.push('false-with-stop');
                return false;
            },
        },
        {
            event: eventType,
            handler: () => {
                calls.push('true-without-stop');
                return true;
            },
        },
        {
            event: eventType,
            stop: true,
            handler: () => {
                calls.push('true-with-stop');
                return true;
            },
        },
        {
            event: eventType,
            handler: () => {
                calls.push('after-stop');
            },
        },
    ]);
    dispatch(document.body, eventType);

    expect(calls).toStrictEqual(['false-with-stop', 'true-without-stop', 'true-with-stop']);
});

test('isolates handler exceptions, warns, and continues dispatch', () => {
    const eventType = 'readout:test:events:exception-isolation';
    const target = document.createElement('button');
    target.dataset.hit = 'true';
    document.body.appendChild(target);
    const failure = new Error('binding exploded');
    const afterFailure = vi.fn();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    registerBindings([
        {
            event: eventType,
            selector: '[data-hit]',
            handler: () => {
                throw failure;
            },
        },
        { event: eventType, selector: '[data-hit]', handler: afterFailure },
    ]);
    dispatch(target, eventType);

    expect(afterFailure).toHaveBeenCalledOnce();
    expect(warn).toHaveBeenCalledOnce();
    expect(warn).toHaveBeenCalledWith(
        'readout event binding failed',
        eventType,
        '[data-hit]',
        failure,
    );
});

test('resolves a Text-node event target to its closest matching element', () => {
    const eventType = 'readout:test:events:text-target';
    const host = document.createElement('button');
    host.dataset.textHost = 'true';
    const text = document.createTextNode('Open');
    host.appendChild(text);
    document.body.appendChild(host);
    const handler = vi.fn();

    registerBindings([{ event: eventType, selector: '[data-text-host]', handler }]);
    dispatch(text, eventType);

    expect(handler).toHaveBeenCalledOnce();
    expect(handler).toHaveBeenCalledWith(expect.any(Event), host);
});

test('routes each event type only to its own ordered binding list', () => {
    const alphaType = 'readout:test:events:type-alpha';
    const betaType = 'readout:test:events:type-beta';
    const calls: string[] = [];

    registerBindings([
        {
            event: alphaType,
            handler: () => {
                calls.push('alpha:first');
            },
        },
        {
            event: betaType,
            handler: () => {
                calls.push('beta');
            },
        },
        {
            event: alphaType,
            handler: () => {
                calls.push('alpha:last');
            },
        },
    ]);

    dispatch(document.body, alphaType);
    expect(calls).toStrictEqual(['alpha:first', 'alpha:last']);

    dispatch(document.body, betaType);
    expect(calls).toStrictEqual(['alpha:first', 'alpha:last', 'beta']);
});
