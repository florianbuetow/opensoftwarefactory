# Open Software Factory agent guidance

Read [INTENT.md](INTENT.md) and [CONSTITUTION.md](CONSTITUTION.md) before any work.

- Manage the local cluster only through justfile targets, never with raw `docker`, `kind`, or `kubectl` commands that start, stop, kill, or delete things. `just help` lists the targets.
- Before any work on the Factory UI, read [the UI design guide](UXDESIGN.md).
- Complete authorized implementation, verification, and local deployment without a permission handoff. Preserve unrelated work. Report actual validation evidence and any remaining gaps.
