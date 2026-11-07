# Contributing

Thank you for working on goat. This page is the whole process; nothing else is required reading before your first pull request.

## The short version

1. Fork, or branch from `main` if you have write access. Branch names: `feat/<topic>`, `fix/<topic>`, `docs/<topic>`.
2. Make the change. Run the gates that apply (below). Add or update the design document in the same change when behaviour or topology changes.
3. Commit with a [Conventional Commits](https://www.conventionalcommits.org/) subject and a DCO sign-off: `git commit -s -m "fix(zenoh): …"`.
4. Open a pull request against `main`. Fill the template. One approving review from a code owner merges it, by squash.

## Commits

- **Conventional Commits.** `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`, with a scope in parentheses naming the component (`feat(goatbuilder): …`). The subject is ≤ 100 characters and says what changed, in the imperative.
- **DCO sign-off on every commit** (`git commit -s`). By signing off you certify the [Developer Certificate of Origin 1.1](https://developercertificate.org/): you wrote the change or have the right to submit it under this project's licence. There is no CLA. The policy is in [Policy](#policy).
- **Signed commits** are required on `main`; set up [SSH or GPG commit signing](https://docs.github.com/en/authentication/managing-commit-signature-verification) once.
- One logical change per pull request. Refactors and behaviour changes are separate pull requests.

## Gates

Every check can be run locally before you push. The scripts are goatcore's: run them from a goatcore checkout beside this one. The ones that apply to most changes:

| Change touches | Run |
|---|---|
| anything | `../goatcore/ops/ci/check-no-identity-spillage.sh` — no keys, addresses or credential values in a committed file |
| Rust (`crates/`) | `cd crates && cargo fmt --check && cargo clippy --workspace -- -D warnings && cargo test --workspace` |
| Go (`ops/*/go.mod`) | `go vet ./... && go test ./...` in that module |
| Ansible | `../goatcore/ops/ci/check-role-template-refs-resolve.sh` · `ansible-playbook --syntax-check` on the playbook you changed |
| Shell | `shellcheck` · `../goatcore/ops/ci/check-bash-portability.sh` (scripts must run on bash 3.2) |

The pull-request workflow runs the same scripts. A red gate names the script, so the fix is reproducible locally. The fuller set of documentation gates, and the decision record, live in [`goatcore`](https://github.com/usareur-af/goatcore).

## Design documents

A change to a component's behaviour, data flow, topology or integration point updates that component's design document under `docs/design/` **in the same pull request**. Each design document states, per part, whether it is *built*, *designed* or *not built*, and the status words mean exactly that:

| Word | Means |
|---|---|
| built | the code exists and runs |
| measured | a command ran and produced the stated result |
| designed | decided, not implemented |
| not built | nothing exists |

A load-bearing decision — one a later contributor would need to know the reason for — is an ADR under `docs/adr/` in [`goatcore`](https://github.com/usareur-af/goatcore).

## Review

Reviewers look for: the change does what its subject says and nothing else; the gates pass; the design document moved with the code; status words are honest; no identifier of a real deployment (an address, a hostname, a key, a person) entered the tree. Review comments are about the change, not the author.

## Merging

Development happens in this repository. A pull request merged to `main` is the change; there is no other upstream. A deployment picks it up by moving the commit it pins, in its own overlay repository.

## Reporting a bug or proposing a feature

Open an issue with the template. For a vulnerability, **do not open an issue** — follow [SECURITY.md](SECURITY.md).

## Licence

Portions written by U.S. Federal government employees within the scope of their employment are not subject to copyright in the U.S.; in countries where copyright is recognized, they and all other contributions are licensed under the [Apache License 2.0](LICENSE). See [INTENT.md](INTENT.md). Contributions are accepted under the [Policy](#policy) below.

**NOTE: This CONTRIBUTING.md is for software contributions. You do not need to follow the Developer's Certificate of Origin (DCO) process for commenting on this repository's documentation, such as CONTRIBUTING.md, INTENT.md, etc. or for submitting issues.**

## Policy

### 1. Introduction

The project maintainer for this Project will only accept contributions using the Developer's Certificate of Origin 1.1 located at [developercertificate.org](https://developercertificate.org) ("DCO"). The DCO is a legally binding statement asserting that you are the creator of your contribution, or that you otherwise have the authority to distribute the contribution, and that you are intentionally making the contribution available under the license associated with the Project ("License").

### 2. Developer Certificate of Origin Process

Before submitting contributing code to this repository for the first time, you'll need to sign a Developer Certificate of Origin (DCO) (see below). To agree to the DCO, add a `Signed-off-by` line to every commit with `git commit -s`. At a high level, signing off tells us that you have the right to submit the work you're contributing and indicates that you consent to our treating the contribution in a way consistent with the license associated with this software (as described in [LICENSE](LICENSE) and [INTENT.md](INTENT.md)) and its documentation ("Project").

### 3. Important Points

Pseudonymous or anonymous contributions are permissible, but you must be reachable at the email address provided in the Signed-off-by line.

If your contribution is significant, you are also welcome to add your name and copyright date to the source file header.

U.S. Federal law prevents the government from accepting gratuitous services unless certain conditions are met. By submitting a pull request, you acknowledge that your services are offered without expectation of payment and that you expressly waive any future pay claims against the U.S. Federal government related to your contribution.

If you are a U.S. Federal government employee and use a `*.mil` or `*.gov` email address, we interpret your Signed-off-by to mean that the contribution was created in whole or in part by you and that your contribution is not subject to copyright protections.

### 4. DCO Text

The full text of the DCO is included below and is available online at [developercertificate.org](https://developercertificate.org):

```txt
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.

Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```
