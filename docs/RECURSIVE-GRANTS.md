# Recursive grants: design note

**Status**: proposed, additive only. No existing grant changes meaning.

## Problem

`Server.Allowed` matches grant roots by exact string:

```go
!slices.Contains(g.Roots, root) && !slices.Contains(g.Roots, "*")
```

An operator who grants `/work/proj` therefore does not grant `/work/proj/pkg`.
Facing that, they do one of three things:

1. Pre-list every subdirectory a tool might touch, which they cannot know in
   advance for a general-purpose tool;
2. Re-grant per call, which does not match how a grant is meant to work;
3. Grant `"*"`, which is far worse than either.

Option 3 is the failure mode worth designing against: an exact-match rule that is
impractical to satisfy pushes operators toward the wildcard.

## Proposal

Add `recursive` to `Grant`, default `false`, opt in per grant:

```
recalldock gateway grant --server X --tool Y --root /work/proj --read-only --recursive
```

`coversRoot` then matches descendants in addition to the exact root.

## What this deliberately does not do

**Existing grants keep their exact-match meaning.** Flipping every stored grant to
recursive would widen access to subdirectories the operator never named, and the
stored record contains no evidence they consented. Any broadening of an existing
grant needs its own explicit act, not an upgrade side effect.

**Matching stops at a path-separator boundary.** `/work/proj` covers
`/work/proj/pkg` but never `/work/project-other`. A bare `strings.HasPrefix`
would match that sibling, which is the classic prefix-confusion bug.

**Windows compares case-insensitively**, mirroring `canonicalPath`, which already
upper-cases the drive letter and resolves symlinks before this runs.

## Open questions for review

1. **Symlinks and junctions.** Both `path` and granted roots arrive already
   canonicalized by `canonicalPath` (EvalSymlinks + Clean), so a recursive grant
   follows the resolved target, not the link. Worth confirming that is the
   intended reading rather than "the literal path as typed".
2. **8.3 short paths on Windows.** `canonicalPath` does not expand them; the
   project layer does that separately (`path_short_windows.go`). If a grant is
   recorded with a short name and a call arrives with the long name, exact match
   already fails today, so this is a pre-existing gap rather than one this change
   introduces. Expanding both sides here would be a separate fix.
3. **Write grants.** Recursion is most useful for read-only grants and most
   dangerous for write grants. Should `--recursive` require `--read-only` in the
   first iteration, and add write recursion only after review? My inclination is
   yes; the code currently does not enforce it.
4. **Path traversal in the argument.** A recursive grant widens the root that the
   gateway accepts, but the backend still receives whatever arguments the model
   sends. If the backend joins the root with a caller-supplied relative path and
   does not itself reject `..`, recursion does not help. That belongs to the
   backend, and is the same boundary noted in the root-scope discussion.

## Not in this PR

`agent`-trust root bounds (rejecting a filesystem root or `$HOME`) are discussed
separately. That change alters what agent trust accepts, so it needs its own
agreement before any code lands; this PR does not touch it.
