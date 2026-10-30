# Contributing

Thanks for looking under the hood. vdb is a thin, replaceable orchestration
layer over Amazon-designated interfaces — keep it that way:

- **Never commit Amazon binaries or SDK material** (repo-lint enforces).
- Every device-facing change lands with: an adapter isolated to one package,
  a fake update, golden fixtures for any parsed format, and tests running
  without hardware.
- `make check` must be green; it includes the e2e loop against fakes.
- Proto changes: edit `proto/vdb/v1/vdb.proto`, run `make generate`, commit
  the regenerated code (the drift gate fails otherwise).
- Keep the smallest private-interface surface; when Amazon ships an
  official equivalent of an adapter's function, prefer theirs.

License: Apache-2.0. Project naming must stay trademark-neutral (no Amazon
marks); the README's unofficial disclaimer stays.
