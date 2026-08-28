// yaml-folds.test.ts -- Vitest for the PURE fold math (yamlEffectiveIndent),
// the load-bearing primitive of buildYamlFolds: it decides which lines OPEN a
// nested block and which are its deeper-indented body. A regression here
// silently changes every fold boundary, so it is pinned directly (no DOM).
//
// Run: `npm test`.

import { expect, test } from 'vitest';

import { planYamlFolds, yamlEffectiveIndent } from './yaml-folds.js';

test('plain leading spaces give the indent depth', () => {
    expect(yamlEffectiveIndent('key: value')).toBe(0);
    expect(yamlEffectiveIndent('  key: value')).toBe(2);
    expect(yamlEffectiveIndent('    nested: x')).toBe(4);
    expect(yamlEffectiveIndent('   ')).toBe(3);
    expect(yamlEffectiveIndent('\tkey: value')).toBe(0);
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

test('only leading newlines are stripped', () => {
    // A later physical line must not be joined onto a space-only prefix and
    // reinterpreted as a sequence item.
    expect(yamlEffectiveIndent('  \n- item')).toBe(2);
});

test('a key whose value happens to start with a dash is NOT a sequence item', () => {
    // The "- " sequence rule keys off the FIRST non-space chars; "key: -1" has a
    // dash inside the value, not at the line head, so it stays at its space depth.
    expect(yamlEffectiveIndent('  replicas: -1')).toBe(2);
});

test('fold planning preserves blank-line boundaries and outer-to-inner ownership', () => {
    const plan = planYamlFolds(
        [0, 2, 2, 4, 0, 2, 0],
        [false, true, false, false, true, false, false],
    );

    expect(plan).toStrictEqual({
        bodyCounts: [3, 0, 1, 0, 0, 0, 0],
        ownersByLine: [[], [], [0], [0, 2], [], [0], []],
    });
});

test('fold planning reads a large flat input only linearly', () => {
    const size = 2_048;
    const readBudget = size * 8;
    let indexedReads = 0;
    const counted = <T>(values: readonly T[]): readonly T[] =>
        new Proxy(values, {
            get(target, property, receiver) {
                if (typeof property === 'string' && Number.isInteger(Number(property))) {
                    indexedReads += 1;
                    if (indexedReads > readBudget) {
                        throw new Error('fold planning exceeded its linear input-read budget');
                    }
                }
                return Reflect.get(target, property, receiver);
            },
        });

    const plan = planYamlFolds(
        counted(new Array<number>(size).fill(0)),
        counted(new Array<boolean>(size).fill(false)),
    );

    expect(plan.bodyCounts).toHaveLength(size);
    expect(plan.ownersByLine).toHaveLength(size);
    expect(plan.bodyCounts.every((count) => count === 0)).toBe(true);
    expect(plan.ownersByLine.every((owners) => owners.length === 0)).toBe(true);
    expect(indexedReads).toBeLessThanOrEqual(readBudget);
});
