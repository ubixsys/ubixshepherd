# Releasing uBixShepherd

GitHub is Shepherd's public home and where releases are published. Development happens
on an internal forge, which push-mirrors protected branches and tags to GitHub. GitHub
Actions builds, signs and publishes each release from its tag. Nobody uploads a binary
by hand.

## The branch model

| Branch | Role |
|---|---|
| `dev` | Where work lands, by merge request, through the gate. |
| `main` | The release branch. Protected, GitHub's default branch, and never pushed to by hand. |

**Promotion.** When the checks pass on a push to `dev`, a job on the internal forge
fast-forwards `main` to that commit (`bin/promote.sh`). It never forces. It does nothing
if `main` already contains the commit, and it fails closed: if it cannot reach the remote
or resolve `main`, it promotes nothing. If `main` is not an ancestor of the commit, `main`
has diverged from `dev` (someone wrote to it directly) and the job refuses and fails.

**Tags are cut on `main`.** The release workflow refuses a tag whose commit is not an
ancestor of `main`, including when it is started by hand.

**The mirror** carries protected branches (`dev` and `main`) and tags to GitHub. Do not
push to GitHub directly. The mirror is a copy: anything on it was already on the internal
forge.

**If promotion stops**, open the `promote-to-main` job on the latest `dev` pipeline:

- *Diverged `main`:* find what is on `main` and not on `dev`. If it should be kept, land
  it on `dev` by merge request (a revert or a merge of `main` into a branch cut from
  `dev`); then promotion fast-forwards on the next green push. Do not force-push `main`.
- *Authentication or branch protection:* the job's token has expired or its role may not
  push to `main`. Refresh the token in the project's CI variables; the job's output names
  which case it is.
- *A pipeline check failed:* promotion waits for green. Fix `dev`.
- *A push was missed or the job was cancelled:* run the pipeline again for the tip of
  `dev`; promotion is idempotent.

Until `main` has caught up, do not tag: the workflow would refuse the tag.

What the version number promises is in [VERSIONING.md](VERSIONING.md).

## What a release publishes

For a tag such as `v0.1.0-beta.1`, the [release workflow](../.github/workflows/release.yml)
creates a GitHub Release holding:

| Asset | What it is |
|---|---|
| `shepherd-<tag>-<os>-<arch>.tar.gz` | Linux and macOS, amd64 and arm64: the binary, `LICENSE` and `README.md` |
| `shepherd-<tag>-windows-<arch>.zip` | The same for Windows, amd64 and arm64 |
| `SHA256SUMS` | SHA-256 of every archive |
| `SHA256SUMS.sigstore.json` | A keyless cosign signature bundle for `SHA256SUMS` |

The release notes are the tag's section of [CHANGELOG.md](../CHANGELOG.md). A tag with a
`-` suffix (`-beta.N`, `-rc.N`) is marked as a pre-release.

The workflow refuses to publish when the binary does not report the tag as its version,
or when the changelog has no section for it. It uses only the built-in `GITHUB_TOKEN`;
signing uses GitHub's OIDC identity, so there is no signing key to keep.

## Cutting a release

1. **Write the changelog section.** On a branch, move what is under `[Unreleased]` into
   a new `## [X.Y.Z] - YYYY-MM-DD` section (the version without the `v`, the date the
   tag will be cut), with a short summary and the **Interface changes** a user has to act
   on. Check that the tooling finds it, and build what the release would publish:

   ```sh
   make release-notes VERSION=vX.Y.Z
   make dist VERSION=vX.Y.Z      # dist/ holds the archives and SHA256SUMS
   make clean
   ```

   Land the branch on `dev` by merge request, through the normal gate and review. If
   Shepherd has a page on ubixsys.com by then, update it in the same pass, so the site
   and the release say the same thing.

2. **Reserve the tag.** In a repo with `tags: reserved`, reserve a final version before
   tagging (`shepherd tag reserve minor --no-lane`, or `patch`, `major`). Reservations
   hand out final versions only; a pre-release tag (`-beta.N`, `-rc.N`) is chosen by
   hand, as the next pre-release of the version it leads to.

3. **Tag `main` and push on the internal forge.** Wait until promotion has put the
   changelog section on `main`, then tag the commit on `main` with an annotated tag and
   push the tag to the internal forge:

   ```sh
   git fetch origin
   git merge-base --is-ancestor <commit-with-the-changelog> origin/main && echo on main
   git tag -a vX.Y.Z -m "uBixShepherd vX.Y.Z" origin/main
   git push origin vX.Y.Z
   ```

   Do not push to GitHub directly. The mirror carries the tag across. The release
   workflow fails early, publishing nothing, if the tagged commit is not on `main`.

4. **Watch the release workflow on GitHub.** When the tag arrives, the Release workflow
   runs. A tag that reaches GitHub through a mirror does not always fire the tag push
   event; if no run appears within a few minutes of the tag showing on GitHub, start it
   by hand against the tag:

   ```sh
   gh workflow run Release --repo ubixsys/ubixshepherd --ref vX.Y.Z
   ```

5. **Check the published release.** Download an archive and verify it as below, and
   confirm the release is marked pre-release if and only if the tag has a suffix.

## A throwaway dry run

To exercise the pipeline without a real release, tag `main` with a throwaway version
such as `v0.0.0-test.1`. A `-` suffix makes it a pre-release. The workflow refuses a tag
with no changelog section, so land a placeholder `## [0.0.0-test.1] - YYYY-MM-DD` section
on `dev` first, wait for promotion, and remove the section again afterwards. Then:

```sh
git fetch origin
git tag -a v0.0.0-test.1 -m "dry run" origin/main
git push origin v0.0.0-test.1
```

Watch the Release workflow, check the release and verify a download as below. Then clean
up, in this order: delete the GitHub Release, then the tag on the internal forge
(`git push origin :refs/tags/v0.0.0-test.1`, and `git tag -d v0.0.0-test.1` locally). The
mirror removes the tag on GitHub, or delete it there if it does not. The signature entry
the run wrote to the public Sigstore transparency log is permanent but harmless: it only
records that the workflow signed a checksum file.

## Verifying a download

Download the archive for your platform, `SHA256SUMS` and `SHA256SUMS.sigstore.json` from
the release into one directory.

First, check that `SHA256SUMS` was signed by this repository's release workflow at that
tag, using [cosign](https://docs.sigstore.dev/cosign/system_config/installation/):

```sh
TAG=v0.1.0-beta.1
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/ubixsys/ubixshepherd/.github/workflows/release.yml@refs/tags/$TAG" \
  SHA256SUMS
```

It prints `Verified OK` only if the signature is valid and the signing certificate was
issued to that workflow file running for that tag.

Then check the archive against the signed checksums:

```sh
sha256sum --ignore-missing -c SHA256SUMS          # Linux
shasum -a 256 --ignore-missing -c SHA256SUMS      # macOS
```

On Windows, compare `Get-FileHash shepherd-<tag>-windows-amd64.zip` with its line in
`SHA256SUMS`.

## When a release goes wrong

- **The workflow failed before publishing** (a tag that does not match the binary, a
  missing changelog section, a build or signing failure): nothing was published. If
  the fix is outside the tagged commit (a runner problem, a flaky step), re-run the
  failed jobs. If the tagged commit itself is wrong, fix it on `dev`, wait for promotion to `main`, and release the
  **next** version. Do not move or re-push a tag that has left the internal forge: the
  mirror and anyone who fetched it already have the old one.
- **Publishing failed after the release was created**, or a re-run finds the release
  already there: delete the partial GitHub Release (not the tag) and re-run the workflow
  against the tag.
- **A published release is broken** (it does not start, or it does something
  dangerous): mark it as a pre-release or edit its notes to say what is wrong and which
  version to use instead, then fix forward with a new version. Delete a release's assets
  only if keeping them is harmful, and say so in the next release's notes. Released
  versions are never re-cut under the same tag.
- **A signature does not verify:** treat the download as untrusted and do not run it.
  Report it to the maintainers through the repository's issue tracker.
