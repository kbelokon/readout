// yaml-folds.test.ts -- node:test for the PURE fold math (yamlEffectiveIndent),
// the load-bearing primitive of buildYamlFolds: it decides which lines OPEN a
// nested block and which are its deeper-indented body. A regression here
// silently changes every fold boundary, so it is pinned directly (no DOM).
//
// Run: `node --test 'internal/assets/src/js/**/*.test.ts'` (Node 24 strips the
// types natively -- no framework, erasable-only TS).

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { yamlEffectiveIndent } from './yaml-folds.ts';

test('plain leading spaces give the indent depth', () => {
    assert.equal(yamlEffectiveIndent('key: value'), 0);
    assert.equal(yamlEffectiveIndent('  key: value'), 2);
    assert.equal(yamlEffectiveIndent('    nested: x'), 4);
});

test('a block-sequence item counts as +2 over its space indent', () => {
    // "- name: x" sits at the same visual column as its parent key, but it
    // structurally nests one level deeper -> indent + 2.
    assert.equal(yamlEffectiveIndent('- name: x'), 2);
    assert.equal(yamlEffectiveIndent('  - name: x'), 4);
    assert.equal(yamlEffectiveIndent('    - image: nginx'), 6);
});

test('a bare dash and a tab-led dash both count as +2', () => {
    assert.equal(yamlEffectiveIndent('-'), 2);
    assert.equal(yamlEffectiveIndent('  -'), 4);
    assert.equal(yamlEffectiveIndent('-\tname'), 2);
});

test('a leading newline is stripped before counting', () => {
    // Pygments line spans carry the trailing newline of the PREVIOUS line as a
    // leading '\n' on textContent; it must not be counted as indentation.
    assert.equal(yamlEffectiveIndent('\n  key: value'), 2);
    assert.equal(yamlEffectiveIndent('\n\n    deep: y'), 4);
});

test('a key whose value happens to start with a dash is NOT a sequence item', () => {
    // The "- " sequence rule keys off the FIRST non-space chars; "key: -1" has a
    // dash inside the value, not at the line head, so it stays at its space depth.
    assert.equal(yamlEffectiveIndent('  replicas: -1'), 2);
});
