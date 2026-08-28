// collapse-hash.ts -- the PURE collapse-section URL-fragment codec.
// Split out of misc-ui.ts so the URL codec stays independently unit-testable and
// free of DOM state. misc-ui.ts reuses parseCollapsedNames from here.
//
// The section-collapse feature round-trips through the URL fragment
// (#collapsed=a,b,c): the .collapsible h4.title write builds
// `collapsed=${names.join(',')}`, and the on-load restore reads it back.

// parseCollapsedNames -- PURE: extract the collapsed-section names from a URL
// hash fragment. The hash is a `;`-separated list of `key=value` params; the
// `collapsed` param's value is a `,`-separated list of section data-name values.
// Returns the names in order, empty when the fragment has no usable `collapsed`.
// No DOM, no decode -- the names are matched verbatim against `[data-name]`.
export function parseCollapsedNames(hash: string): string[] {
    const names: string[] = [];
    // Drop only the fragment marker at the beginning; a '#' inside a name is
    // data and must survive verbatim. Each param is split on '=' and only the
    // `collapsed` key's value (the element at index 1) contributes names.
    hash.replace(/^#/, '')
        .split(';')
        .forEach((param) => {
            const keyVal = param.split('=');
            if (keyVal[0] === 'collapsed' && keyVal[1]) {
                keyVal[1].split(',').forEach((name) => {
                    if (name) {
                        names.push(name);
                    }
                });
            }
        });
    return names;
}
