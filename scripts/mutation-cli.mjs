const defaultFullConcurrency = 4;
const defaultMaximumConcurrency = 4;

function optionValue(args, index, optionName) {
  const argument = args[index];
  const equalsIndex = argument.indexOf('=');
  if (equalsIndex !== -1) {
    const value = argument.slice(equalsIndex + 1);
    if (value.length === 0) throw new Error(`${optionName} requires a value`);
    return { value, consumed: 0 };
  }

  const value = args[index + 1];
  if (value === undefined || value.startsWith('-')) {
    throw new Error(`${optionName} requires a value`);
  }
  return { value, consumed: 1 };
}

/**
 * Parse the launcher's deliberately small CLI instead of forwarding arbitrary
 * Stryker options. The selected mode determines the only Stryker arguments the
 * launcher constructs: a dry run gets --dryRunOnly; a full run never does.
 */
export function parseMutationCliArguments(
  args,
  { maximumConcurrency = defaultMaximumConcurrency } = {},
) {
  let mode;
  let concurrency;

  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === '--mode' || argument.startsWith('--mode=')) {
      if (mode !== undefined) throw new Error('--mode may be specified only once');
      const parsed = optionValue(args, index, '--mode');
      mode = parsed.value;
      index += parsed.consumed;
      continue;
    }

    if (argument === '--concurrency' || argument.startsWith('--concurrency=')) {
      if (concurrency !== undefined) {
        throw new Error('mutation concurrency may be specified only once');
      }
      const parsed = optionValue(args, index, '--concurrency');
      concurrency = parsed.value;
      index += parsed.consumed;
      continue;
    }

    if (argument === '-c') {
      if (concurrency !== undefined) {
        throw new Error('mutation concurrency may be specified only once');
      }
      const parsed = optionValue(args, index, '-c');
      concurrency = parsed.value;
      index += parsed.consumed;
      continue;
    }

    if (/^-c(?:=)?.+/u.test(argument)) {
      if (concurrency !== undefined) {
        throw new Error('mutation concurrency may be specified only once');
      }
      concurrency = argument.slice(2).replace(/^=/u, '');
      continue;
    }

    if (argument.startsWith('-')) {
      throw new Error(`unsupported mutation launcher option: ${argument}`);
    }
    throw new Error(`unexpected positional mutation argument: ${argument}`);
  }

  if (mode !== 'dry' && mode !== 'full') {
    throw new Error('--mode must be exactly "dry" or "full"');
  }

  const parsedConcurrency =
    concurrency === undefined ? (mode === 'dry' ? 1 : defaultFullConcurrency) : Number(concurrency);
  if (
    !Number.isInteger(parsedConcurrency) ||
    parsedConcurrency < 1 ||
    parsedConcurrency > maximumConcurrency
  ) {
    throw new Error(`mutation concurrency must be an integer from 1 to ${maximumConcurrency}`);
  }

  return {
    concurrency: parsedConcurrency,
    mode,
    strykerArgs: [
      ...(mode === 'dry' ? ['--dryRunOnly'] : []),
      '--concurrency',
      String(parsedConcurrency),
    ],
  };
}
