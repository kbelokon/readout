// @vitest-environment jsdom

import { screen } from '@testing-library/dom';
import { beforeEach, describe, expect, test, vi } from 'vitest';

import { showToast } from './toasts.js';

describe('showToast', () => {
    beforeEach(() => {
        vi.useFakeTimers();
    });

    test('does nothing when the toast host is absent', () => {
        expect(() => showToast('Saved')).not.toThrow();
        expect(document.body).toBeEmptyDOMElement();
    });

    test('renders untrusted text without interpreting markup', () => {
        document.body.innerHTML = '<div id="ro-toasts"></div>';

        showToast('<img src=x onerror=alert(1)>');

        expect(screen.getByText('<img src=x onerror=alert(1)>')).toHaveClass('ro-toast');
        expect(document.querySelector('img')).not.toBeInTheDocument();
    });

    test('enters the leaving state and then removes the toast', async () => {
        document.body.innerHTML = '<div id="ro-toasts"></div>';
        showToast('Refresh resumed');
        const toast = screen.getByText('Refresh resumed');

        await vi.advanceTimersByTimeAsync(3499);
        expect(toast).not.toHaveClass('is-leaving');

        await vi.advanceTimersByTimeAsync(1);
        expect(toast).toHaveClass('is-leaving');
        expect(toast).toBeInTheDocument();

        await vi.advanceTimersByTimeAsync(200);
        expect(toast).not.toBeInTheDocument();
    });

    test('keeps independent lifetimes for multiple notifications', async () => {
        document.body.innerHTML = '<div id="ro-toasts"></div>';
        showToast('First');
        await vi.advanceTimersByTimeAsync(1000);
        showToast('Second');

        await vi.advanceTimersByTimeAsync(2600);
        expect(screen.getByText('First')).toHaveClass('is-leaving');
        expect(screen.getByText('Second')).not.toHaveClass('is-leaving');

        await vi.advanceTimersByTimeAsync(100);
        expect(screen.queryByText('First')).not.toBeInTheDocument();
        expect(screen.getByText('Second')).toBeInTheDocument();
    });
});
