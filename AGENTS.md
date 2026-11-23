# Open Software Factory agent guidance

Read [INTENT.md](INTENT.md) and [CONSTITUTION.md](CONSTITUTION.md) before any work.

- Manage the local cluster only through justfile targets, never with raw `docker`, `kind`, or `kubectl` commands that start, stop, kill, or delete things. `just help` lists the targets.
- Before any work on the Factory UI, read [the UI design guide](UXDESIGN.md).
- Never test with the seed workflows, seed agents, or their runs: seeds are not tracked in git, so a test cannot rely on them existing. Every test defines and sets up its own test cases (its own agents, workflows, inputs, and runs) and tears all of them down afterwards, leaving the factory as it found it.
- Complete authorized implementation, verification, and local deployment without a permission handoff. Preserve unrelated work. Report actual validation evidence and any remaining gaps.
