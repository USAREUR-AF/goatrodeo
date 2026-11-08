# CI standards

Built: workflows pin external actions to commit SHAs and state their token permissions. Dependabot proposes weekly updates for the configured ecosystems; maintainers review these updates before merging.

Built: the DCO workflow checks every commit introduced by a pull request, including merge commits. Each commit needs a parsed `Signed-off-by: Name <email>` trailer matching its author. An email alias alone does not prove a sign-off. Only Dependabot's own author identity on a pull request opened by Dependabot is exempt. This checks the declaration, not the legal identity of its author.

Create signed, signed-off commits with `git commit -S -s`. To correct your latest commit, use `git commit -S --amend --no-edit -s` with the correct author identity. Review history changes with collaborators before pushing them.

Run the offline regression tests with:

```bash
python3 .github/scripts/test_dco.py
```

Built: Scorecard runs on the default branch and publishes public supply-chain results, with SARIF uploaded to code scanning. Hosted publication and dependency-update execution still need verification after merge. These workflows do not configure branch protection or make their jobs required checks. Maintainers choose required checks after confirming successful hosted runs.

The DCO workflow uses the ordinary pull-request event with a read-only token and no persisted checkout credentials. Changes to the workflow or checker require maintainer review; the checker is not independent of the proposed source.
