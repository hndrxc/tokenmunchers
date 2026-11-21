# Repository instructions

## Commits and Munch releases

- Commit only the requested changes. In omp, use `commit.draft()` in eval, inspect the selection, and call `d.commit(...)`. Do not use `git add` or `git commit`; the draft keeps unrelated staged and unstaged work out of your commit.
- When asked to cut a Munch release, run `bash .github/scripts/release-munch <major.minor.patch>` from the repository root on `main`. It pushes the release commit to `origin/main` before pushing the `munch-v*` tag on its own. Never combine the branch and tag pushes or use `--follow-tags` for this release: GitHub can run branch CI without starting the tag workflow.
- Confirm the tag's `munch` workflow finished and the GitHub Release has all four archives and `SHA256SUMS`. Use the [release instructions](proxy/README.md#cut-a-release) to recover a missing tag run.
