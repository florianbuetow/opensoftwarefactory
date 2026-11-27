# Open Software Factory agent guidance

Read [INTENT.md](INTENT.md) and [CONSTITUTION.md](CONSTITUTION.md) before any work.

- Manage the local cluster only through justfile targets, never with raw `docker`, `kind`, or `kubectl` commands that start, stop, kill, or delete things. `just help` lists the targets.
- Helper scripts live in [scripts/](scripts/); run them as `./scripts/<name>.sh` from the repository root. `just e2e` (`./scripts/e2e.sh`) runs the end-to-end tests, which need the running factory and fail unless it is fully up; `./scripts/integration.sh` runs the integration tests, which need no cluster.
- Before any work on the Factory UI, read [the UI design guide](UXDESIGN.md).
- Test only with the agents and workflows of the group `test` in [fixtures/factory](fixtures/factory/README.md); add new test cases there. A test starts its own runs and ends with `./scripts/e2e.sh ui-sweep`, which removes every test run and fails if one is left.
- Complete authorized implementation, verification, and local deployment without a permission handoff. Preserve unrelated work. Report actual validation evidence and any remaining gaps.
- Never attribute the AI in any git commit: no `Co-Authored-By`, `Generated with`, or similar trailer, and no AI name in commit messages or authorship.
