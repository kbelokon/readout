// Stryker 10 still uses the TypeScript 6 API while preparing its sandbox and
// bootstrapping the experimental native TypeScript 7 checker. Redirect only
// those tooling imports; Vitest and the readout project keep resolving TS 7.

import { registerHooks } from 'node:module';

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (
      specifier === 'typescript' &&
      (context.parentURL?.includes('/node_modules/@stryker-mutator/core/') ||
        context.parentURL?.includes('/node_modules/@stryker-mutator/typescript-checker/'))
    ) {
      return nextResolve('@typescript/legacy', context);
    }
    return nextResolve(specifier, context);
  },
});
