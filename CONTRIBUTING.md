# Contributing to zonegit

Thanks for your interest. zonegit is a small, pre-1.0 project, and the
fastest path to a merged PR is a short conversation up front. The
guidelines below are meant to keep that conversation cheap.

## Before you start

- For anything bigger than a typo or a one-file fix, please open an
  issue first so we can agree on the shape. This avoids the "great PR,
  wrong direction" outcome that is unfair to everyone.
- Check [docs/ROADMAP.md](docs/ROADMAP.md) — work that aligns with the
  current milestone is much more likely to land quickly.
- Check the [`good first issue`](https://github.com/ckumar392/zonegit/labels/good%20first%20issue)
  label if you want a starting point.

## Local setup

You need Go 1.23+ and `make`. Optional: `golangci-lint` (recommended)
and `dig` (for the demo).

```sh
git clone https://github.com/ckumar392/zonegit.git
cd zonegit
make build      # produces ./bin/zonegit and ./bin/zonegitd
make test       # unit tests
make test-race  # tests with the race detector
make lint       # golangci-lint, falls back to go vet
make demo       # end-to-end demo
```

## Making a change

1. Fork, create a branch off `main`. Branch names are not policed; pick
   something short and descriptive (`fix-blame-empty-tree`,
   `feat-axfr`).
2. Keep the change focused. One logical change per PR. If you find
   yourself writing "and also" in the description, that's two PRs.
3. Add or update tests. New behaviour without a test is unlikely to
   merge. Bug fixes should include a regression test that fails before
   the fix.
4. Run the full local check before pushing:
   ```sh
   make lint test-race
   ```
5. Update [CHANGELOG.md](CHANGELOG.md) under the `## [Unreleased]`
   section. One bullet, past tense, user-visible language.

## Commit style

- Short, lowercase subject line, no trailing period. Imperative or
  past-tense both fine — match the surrounding history.
- Optional body explaining the *why*, wrapped at ~72 columns.
- One commit per logical change is preferred but not required; we
  squash-merge if the history is messy.

Good:
```
refs: reject HEAD updates that would create a cycle

CASRef previously trusted the caller to detect cycles. It can't —
the storage layer is the only place with a global view of the ref
graph. Move the check there, with a regression test.
```

## Code style

- Standard Go formatting. `gofmt` and `goimports` are enforced by CI;
  run `make lint` locally.
- Exported types and functions should have doc comments when their
  purpose is non-obvious. We are not pedantic about this for internal
  types yet.
- Keep packages cohesive. If a change touches `pkg/object`,
  `pkg/refs`, and `pkg/zone` in roughly equal proportion, that's a
  smell worth discussing in the issue before coding.
- Prefer the `Storage` interface in [pkg/store](pkg/store) over any
  concrete backend. Tests should run against
  [pkg/store/memstore](pkg/store/memstore) unless they are
  specifically exercising Badger.

## Tests

- Unit tests live next to the code (`foo_test.go`).
- Storage backends MUST pass the conformance suite in
  [pkg/store/storetest](pkg/store/storetest). If you add a new backend,
  wire it in — see [pkg/store/badger/conformance_test.go](pkg/store/badger/conformance_test.go)
  for the pattern.
- Anything touching concurrency must pass `make test-race`.

## Pull requests

- Fill in the PR template. The "what / why / how tested" structure is
  not bureaucracy; it is the minimum a reviewer needs to evaluate the
  change.
- Link the issue with `Fixes #N` or `Refs #N`.
- Expect feedback. We review for correctness, scope, and long-term
  maintainability, in that order.
- Once approved, a maintainer will merge. We squash-merge by default.

## Releases

Tagged releases follow [Semantic Versioning](https://semver.org/).
Pre-1.0 means the on-disk format and public API may break between
minor versions; we will call it out loudly in the changelog when they
do.

## License

By contributing you agree that your contribution will be licensed
under the [Apache License 2.0](LICENSE), the same as the rest of the
project.

## Code of Conduct

Participation is governed by the
[Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).
