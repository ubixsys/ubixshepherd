# Versioning

uBixShepherd follows [Semantic Versioning](https://semver.org/). This page says what the
number promises, and what it does not.

## What the version communicates

The version communicates one thing: **the stability of Shepherd's public interfaces**.
Those are:

| Interface | What it covers |
|---|---|
| The CLI | `shepherd` commands, their flags, and output that scripts are expected to read |
| The config file schema | `~/.shepherd/config.yaml`: daemon settings, defaults and repo profiles |
| The HTTP API | The daemon's loopback API that the CLI, the MCP server and other clients use |
| The MCP tools | The operator set (`shepherd mcp`) and the worker set (`shepherd mcp --worker`): tool names, arguments and results |
| The hook protocol | The pre-push hook Shepherd installs, the line it asks other hooks to chain, and what the check accepts and refuses |

For a released version:

- **`MAJOR`** (`1.x` to `2.0`): a backward-incompatible change to one of those interfaces.
- **`MINOR`** (`1.0` to `1.1`): new capability that keeps existing use working.
- **`PATCH`** (`1.0.0` to `1.0.1`): fixes that keep existing use working.
- **Pre-release** (`-beta.N`, `-rc.N`): a candidate for the version it precedes, with
  lower precedence than that version.

Shepherd's own store (the SQLite file in `~/.shepherd`) is not a public interface, but it
upgrades in place: schema changes are forward-only migrations applied when the daemon
starts. A binary refuses a store migrated by a newer one, so going back a version can
mean starting from a fresh store.

## What `0.x` and `-beta.N` mean here

Shepherd is in `0.x`. Under SemVer, anything may change in `0.x`; in practice we hold
ourselves to this:

- **`0.x.y-beta.N`**: a beta of `0.x.y`. **Interfaces may still change between betas.**
  Every change to one of the interfaces above is called out in [CHANGELOG.md](../CHANGELOG.md)
  under its own heading, with what to do about it, so an upgrade is never a surprise.
- **`0.x.y-rc.N`**: a release candidate. No interface changes are planned before
  `0.x.y`; one that has to happen is called out the same way.
- **`0.x.y`**: interface changes inside a `0.x` line are avoided in patch releases. A
  change that breaks existing use moves the minor number (`0.1` to `0.2`).
- **`1.0.0`** will mean the interfaces above are stable and the SemVer compatibility
  rules apply from then on.

The first release is **`v0.1.0-beta.1`**.

## What the version does not communicate

The version says nothing about **security assurance**. It is not a security review, a
production-readiness stamp, or a claim that Shepherd's guards cannot be bypassed. Shepherd
starts AI agents on your machine with your credentials, and some of its limits are
enforced by mechanisms an agent could step around (the changelog's "Known limits" says
which). Weigh that before giving agents more autonomy in a repo, whatever the version
number says. A security review, when one happens, will be stated in the changelog and the
README; it is not a version gate, and no version bump is implied by it.

## Tags

Release tags are `v` followed by the version (`v0.1.0-beta.1`). A tag's binaries report
exactly that version (`shepherd version`), and the release workflow refuses a tag that
does not match, or one with no section in the changelog. See
[RELEASING.md](RELEASING.md).
