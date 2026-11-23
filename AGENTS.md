# Open Software Factory agent guidance

Read [INTENT.md](INTENT.md) and [CONSTITUTION.md](CONSTITUTION.md) before any work.

- Manage the local cluster only through justfile targets, never with raw `docker`, `kind`, or `kubectl` commands that start, stop, kill, or delete things. `just help` lists the targets.
- Helper scripts live in [scripts/](scripts/); run them as `./scripts/<name>.sh` from the repository root. `./scripts/verify.sh` lists the live verification checks when run without a check.
- Before any work on the Factory UI, read [the UI design guide](UXDESIGN.md).
- Test only with the agents and workflows of the group `test` in [fixtures/factory](fixtures/factory/README.md); add new test cases there. A test starts its own runs and ends with `./scripts/verify.sh ui-sweep`, which removes every test run and fails if one is left. `./scripts/verify.sh ui-live` runs the test workflows through the live factory and sweeps.
- Complete authorized implementation, verification, and local deployment without a permission handoff. Preserve unrelated work. Report actual validation evidence and any remaining gaps.
