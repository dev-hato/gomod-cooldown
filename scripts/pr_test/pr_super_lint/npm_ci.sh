#!/usr/bin/env bash
set -e

bash "${GITHUB_WORKSPACE}/scripts/npm_ci.sh"
action="$(yq '.jobs.pr-super-lint.steps[-1].uses | line_comment' .github/workflows/pr-test.yml)"
PATH="$(docker run --rm --entrypoint '' "ghcr.io/super-linter/super-linter:slim-${action}" /bin/sh -c 'echo $PATH')"
echo "PATH=/github/workspace/node_modules/.bin:${PATH}" >>"$GITHUB_ENV"
