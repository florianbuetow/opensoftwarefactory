# Test fixtures

The agents and workflows every test uses, under the group `test`:

- `registry/agents/test/<name>/v1/`: test agents (`agent.json`, `systemprompt.md`).
- `orchestrator/test.json`: test workflows, using only test agents.

`just deploy` publishes them to the factory. Add a new test case here as a new agent or workflow under `test/`. A test starts its own runs of these workflows and ends with `./scripts/e2e.sh ui-sweep`, which removes every test run and fails if one is left; `./scripts/e2e.sh ui` runs them through the running factory and sweeps.
