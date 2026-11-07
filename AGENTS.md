# AGENTS.md

Instructions for AI coding agents, and a short version for anyone working here. [CONTRIBUTING.md](CONTRIBUTING.md) is the full process.

## What this repository is

goatrodeo holds games and toys that run on a goat data fabric. Each one publishes real events onto the fabric, so it doubles as a traffic generator you can watch in flashlight, panoscope or `goat sub`. None of it is needed to run a goatnet.

It sits beside [goatcore](https://github.com/usareur-af/goatcore), which builds and runs a goatnet. A real deployment is goatcore at a pinned commit plus a private overlay repository with that deployment's configuration; no deployment's configuration is ever in this repository.

## Layout

| Path | What |
|---|---|
| `ops/serve/` | each game's service code and Dockerfile |
| `ops/zenoh-trial-peers/` | the fabric side of each game (schemas, sidecars) |
| `ops/ansible/` | the roles that deploy the games |
| `docs/design/`, `docs/operations/` | how each game works and how it is run |

## Rules CI enforces

1. **No identifier of a real deployment.** Hostnames, domains, addresses, account ids, keys, people, places and organisations of any real goatnet never enter a file or a commit message. Use the example goatnet `zagros`, documentation addresses (RFC 5737, and `100.127.0.0/16` for mesh), and `example.org` mail. The leak gate (`.github/workflows/leak-gate.yml`) checks every added line against a private list of deployment names. It never prints what matched, so a red leak gate means: rename the identifier. The name list is not available outside CI.
2. **Commits.** Conventional Commits subject of 100 characters or fewer, with a scope (`fix(<component>): …`). Every commit is signed off (`git commit -s`, the DCO) and signed. One logical change per pull request.
3. **Design documents move with the code.** A change to behaviour, data flow, topology or an integration point updates its design document in the same pull request, using the status words built, measured, designed and not built exactly. Decisions are ADRs in [goatcore](https://github.com/usareur-af/goatcore/blob/main/docs/adr/INDEX.md).
4. **A file at the same path as one in goatcore stays byte-identical to it.** Deployments assemble goatcore, goatstuff and goatrodeo into one tree, and the assembly stops on any path two of them carry with different bytes (the exceptions are each repository's root documents, `.github/`, `crates/Cargo.toml`, `crates/Cargo.lock` and `docs/RELEASE-CONTENTS.md`). Change such a file in goatcore first, then copy it here unchanged. To list them: `comm -12 <(git ls-files | sort) <(git -C ../goatcore ls-files | sort)`.

## Before you push

The check scripts live in goatcore. With a goatcore checkout beside this one:

```bash
bash ../goatcore/ops/ci/check-no-identity-spillage.sh
bash ../goatcore/ops/ci/check-bash-portability.sh
```

## Read first

- [architecture-spine.md](https://github.com/usareur-af/goatdocs/blob/main/docs/design/architecture-spine.md) in goatdocs: the architecture in one read.
