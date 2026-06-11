// collapse-hash.ts -- the PURE collapse-section URL-fragment codec (Unit 9).
// Split out of misc-ui.ts so it has NO runtime imports: Node's native
// type-stripping (`node --test`) resolves `.js` specifiers literally and cannot
// follow a runtime `./x.js` import to its `.ts` source, so a node-tested module
// must stay free of runtime cross-module imports. misc-ui.ts (which DOES carry
// runtime imports for its bindings) re-uses parseCollapsedNames from here.
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
    if (!hash) {
        return [];
    }
    const names: string[] = [];
    // substring(1) drops a leading '#' the same way the monolith did; a
    // fragment with no '#' is parsed verbatim. Each param is split on '=' and
    // only the `collapsed` key's value (the element AT index 1, mirroring the
    // monolith's keyVal[1]) contributes names.
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
