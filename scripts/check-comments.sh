#!/usr/bin/env bash
# comment-check: fail when code comments cite internal design entities
# (decision codes like D4, plan units, backlog ids, SPEC sections, bare
# section signs like §1.3, design docs) instead of explaining the invariant
# in plain words. A comment must
# stand on its own: the reader has the code and the comment, not the
# planning archive.
#
# False-positive rule: if a legitimate comment ever trips this guard, fix it
# by narrowing the pattern or the comment-context detection HERE -- never by
# deleting the comment's substance.
set -u

# Tokens that mean "go read a design doc".
markers='\bD[0-9]+[a-z]?\b|\b(Unit|U) ?[0-9]+\b|\bB-[0-9]{3}\b|SPEC|§[0-9]|design\.md|docs/forge|design_handoff'

# Comment-context line shapes for Go/templ/TS/JS: line comments, block-comment
# openers, block continuations with a leading *, and HTML comments in templ.
comment_ctx='(//|/\*|^[[:space:]]*\*|<!--)'

violations=$(
  {
    grep -rEn "${comment_ctx}.*(${markers})" \
      --include='*.go' --include='*.templ' --include='*.ts' --include='*.js' \
      --exclude-dir=node_modules \
      internal/ cmd/ tests/ 2>/dev/null
    # CSS has no line shape where these tokens are legitimate code, so every
    # line is scanned -- this also catches bare lines inside /* */ blocks.
    grep -rEn "(${markers})" \
      --include='*.css' \
      --exclude-dir=node_modules \
      internal/ cmd/ tests/ 2>/dev/null
  } | sort -u
)

if [ -n "${violations}" ]; then
  printf '%s\n' "${violations}"
  echo 'comment-check: design-doc references found in comments (D-codes, SPEC, §N section signs, docs/forge); explain the invariant in plain words instead' >&2
  exit 1
fi
exit 0
