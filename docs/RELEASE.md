# Building, Packaging & Upgrading RecallDock Releases

RecallDock packages are offline bundles containing the compiled binary, host plugin assets, installation/rollback scripts, the member integrity manifest (`release.json`), build provenance (`build.json`), and archive SHA256 checksums (`checksums.txt`). Build tools do not upload packages or change installed hosts or databases.

## 1. Development Candidates vs. Formal Releases

- Development candidates require `--development` and a prerelease version such as `0.3.24-dev.1`. They carry `development: true`, `published: false`, and cannot pass formal publication verification. Dirty source is allowed only for isolated development tests; release-readiness candidates should use clean committed source.
- Formal packages require a clean checkout and `--publish-local`, a final version such as `0.3.24`, and an existing annotated tag `v0.3.24` pointing to `HEAD`. Missing, lightweight, mismatched, or changed tags stop the build.
- Formal Linux amd64 and Windows amd64 packages must have identical `version`, `source_commit`, `source_tag`, and `source_tag_object`, with `source_dirty: false` and `development: false`. `published: true` in these manifests means a verified local offline artifact, not a public GitHub Release.
- Never rename development archives into formal releases. After approval, rebuild both platforms from the same annotated tag.

### Linux (`x86_64` / `amd64`)

```sh
# Isolated development candidate; output must be a new absolute directory
python3 -I -B tools/build-release.py --version 0.3.24-dev.1 --development --output /tmp/recalldock-linux-dev

# Formal build, only after the annotated tag has been approved and created
python3 -I -B tools/build-release.py --version 0.3.24 --publish-local --output /tmp/recalldock-linux-release
```

### Windows (`x64` / `amd64`)

The Windows ZIP can be cross-compiled on Linux or built natively on Windows. Cross-compilation is not native Windows acceptance; `tools/verify-windows.py` must run on Windows.

```sh
python3 -I -B tools/build-release-windows.py --version 0.3.24-dev.1 --development --output /tmp/recalldock-windows-dev
python3 -I -B tools/build-release-windows.py --version 0.3.24 --publish-local --output /tmp/recalldock-windows-release
```

### Integrity Verification

`tools/verify-release.py` independently checks the checkout/tag identity, both `build.json` files, the `release.json` embedded in each archive, the exact member inventories and member SHA256s, and the archive SHA256s against both `build.json` and `checksums.txt`. It rejects missing attribution/runtime files, unexpected members, archive links, and mismatched platform/version/provenance flags. It reads archive members without extracting or executing them.

```sh
COMMIT=$(git rev-parse HEAD)
TAG_OBJECT=$(git rev-parse refs/tags/v0.3.24)
python3 -I -B tools/verify-release.py --version 0.3.24 \
  --source-commit "$COMMIT" --tag-object "$TAG_OBJECT" \
  --linux /tmp/recalldock-linux-release --windows /tmp/recalldock-windows-release \
  --checksums-output /tmp/recalldock-checksums.txt --report /tmp/recalldock-release-verification.json
```

For clean development candidates, use the same verifier with `--development`, the exact prerelease version and source commit, and no `--tag-object`. This verifies candidates but never authorizes publication.

## 2. Public Source Export & Verification

The local source checkout is authoritative. Commit and test fixes there first, then export the public allowlist into a fresh directory, audit it, and synchronize the existing Private preview. Do not export private development Git history, local instructions, workbooks, credentials, or databases.

```sh
python3 -I -B tools/export-public.py --output /tmp/recalldock-public-snapshot
python3 -I -B tools/export-public.py --check --target /tmp/recalldock-public-snapshot
```

`--check --target .` is appropriate in the clean exported public checkout, not in the private authoritative checkout containing intentionally excluded files. Compare every exported file's bytes and executable mode with the authoritative allowlist before synchronization.

The local development commit and the public snapshot commit have different Git identities because private history and files are excluded. Record both commits and their verified export mapping. Release manifests from GitHub Actions refer to the tagged **public source commit**; do not claim it is the private development commit.

## 3. Manual GitHub Release Workflow

`.github/workflows/release.yml` has only `workflow_dispatch`; pushes and tags do not publish releases. The `publish` input defaults to `false`. The workflow never creates tags, changes repository visibility, or publishes a non-draft Release.

1. Obtain explicit operator approval for the exact source mapping, target repository, annotated tag, public visibility, and formal publication. Preserve the stable `v0.3.23` tag and installed runtime.
2. After approval, create the annotated `v0.3.24` tag on the verified public snapshot commit (and record the corresponding local source commit). Dispatch the workflow on that tag with `version=0.3.24`.
3. The resolver verifies the annotated tag and captures its commit and tag-object SHA once. Both platform jobs check out that pinned commit, validate that the tag has not changed, and build with `--publish-local`.
4. Linux runs core/installer tests and isolated installation, retrieval, project-boundary, Gateway, Work, admission and host-adapter checks. Windows runs native core tests and the isolated Windows ZIP verification, including PowerShell install/rollback and database preservation.
5. Both platform jobs verify their own manifests and checksums. An independent job downloads both archives, checks their shared source/version/tag identity, and emits the combined checksums and verification report. Any failure prevents the draft job from running. Windows explicitly stops on each failed external command.
6. With `publish=false`, only Actions artifacts are produced. With `publish=true`, the final job rechecks the downloaded packages before creating a **draft** Release using `--verify-tag --draft`. Assets include both archives, combined checksums, uniquely named platform build manifests, and verification reports.
7. Only under the operator's explicit approval, publish the verified draft and change the approved repository's visibility. Re-download assets into new isolated directories and verify SHA256, tag object, tagged public source commit, manifest versions and executable versions before declaring publication complete.

The ordinary `.github/workflows/ci.yml` tests preview branches and runs native Windows candidate acceptance; preview artifacts do not count as formally tagged release packages.

## 4. Upgrade Guarantees

- **Digest-CAS Host Edits**: Installers compute `plan_sha256` over before/after file hashes and file modes, locking configuration targets before writing so concurrent edits fail safely.
- **Hardened Policy Retention**: During installation/upgrade, legacy `candidate-admission.json` policies (`checked`, `duplicates`, `tiered-v3`) are hardened to `checked-v2` / `duplicates-v2` / `tiered-v4` and marked `retain_on_rollback: true`; rollback never silently restores legacy unverified admission.
- **Database Preservation**: Neither installer nor rollback modifies `recalldock.db` or `operations.db`. Before a production upgrade, snapshot both databases (`recalldock backup --json` and `recalldock work backup`). This release-preparation workflow uses isolated test data, not production databases.

For installation and recovery instructions, see [INSTALLATION.md](INSTALLATION.md) and [WINDOWS.md](WINDOWS.md).
