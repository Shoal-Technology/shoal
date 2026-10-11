## Summary

<!-- What changes, and why. Link the issue if there is one. -->

## Type of change

- [ ] Bug fix
- [ ] Feature
- [ ] Refactor or cleanup (no behaviour change)
- [ ] Documentation
- [ ] ADR (new or changed decision in `docs/decisions/`)

## Checklist

- [ ] Every commit is signed off with `git commit -s` (DCO, see CONTRIBUTING.md).
- [ ] `make test` and `make vet` pass locally; `gofmt -l .` prints nothing.
- [ ] Tests cover the change. A bug fix includes a test that fails without it.
- [ ] If a `.proto` file changed, `make proto` was run and the generated files are committed.
- [ ] README or `docs/` is updated when behaviour, flags or the install path change.
- [ ] If a dependency was added or updated, `python3 hack/license_check.py --notice NOTICE` passes and `NOTICE` is committed.
- [ ] An ADR is added or updated if the design, on-disk format, wire protocol or operator contract changed.
- [ ] I have not run integration or e2e tests on a machine that is not disposable.

## Notes for the reviewer

<!-- Anything risky, anything you were unsure about, and how you tested it. -->
