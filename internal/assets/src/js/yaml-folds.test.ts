// yaml-folds.test.ts -- Vitest for the PURE fold math (yamlEffectiveIndent),
// the load-bearing primitive of buildYamlFolds: it decides which lines OPEN a
// nested block and which are its deeper-indented body. A regression here
// silently changes every fold boundary, so it is pinned directly (no DOM).
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import { yamlEffectiveIndent } from './yaml-folds.js';

test('plain leading spaces give the indent depth', () => {
    expect(yamlEffectiveIndent('key: value')).toBe(0);
    expect(yamlEffectiveIndent('  key: value')).toBe(2);
    expect(yamlEffectiveIndent('    nested: x')).toBe(4);
});

test('a block-sequence item counts as +2 over its space indent', () => {
    // "- name: x" sits at the same visual column as its parent key, but it
    // structurally nests one level deeper -> indent + 2.
    expect(yamlEffectiveIndent('- name: x')).toBe(2);
    expect(yamlEffectiveIndent('  - name: x')).toBe(4);
    expect(yamlEffectiveIndent('    - image: nginx')).toBe(6);
});

test('a bare dash and a tab-led dash both count as +2', () => {
    expect(yamlEffectiveIndent('-')).toBe(2);
    expect(yamlEffectiveIndent('  -')).toBe(4);
    expect(yamlEffectiveIndent('-\tname')).toBe(2);
});

test('a leading newline is stripped before counting', () => {
    // Pygments line spans carry the trailing newline of the PREVIOUS line as a
    // leading '\n' on textContent; it must not be counted as indentation.
    expect(yamlEffectiveIndent('\n  key: value')).toBe(2);
    expect(yamlEffectiveIndent('\n\n    deep: y')).toBe(4);
});

test('a key whose value happens to start with a dash is NOT a sequence item', () => {
    // The "- " sequence rule keys off the FIRST non-space chars; "key: -1" has a
    // dash inside the value, not at the line head, so it stays at its space depth.
    expect(yamlEffectiveIndent('  replicas: -1')).toBe(2);
});
