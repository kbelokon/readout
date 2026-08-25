import '@testing-library/jest-dom/vitest';

import { afterEach, vi } from 'vitest';

afterEach(() => {
    if (vi.isFakeTimers()) {
        vi.clearAllTimers();
        vi.useRealTimers();
    }

    if (typeof document === 'undefined') {
        return;
    }

    document.head.replaceChildren();
    document.body.replaceChildren();
    for (const part of document.cookie.split(';')) {
        const name = part.split('=', 1)[0]?.trim();
        if (name) {
            document.cookie = `${name}=; Path=/; Max-Age=0`;
        }
    }
    document.documentElement.removeAttribute('class');
    document.documentElement.removeAttribute('data-theme');
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.history.replaceState(null, '', '/');
});
