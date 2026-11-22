# Intent

## What it is

A thin software factory: a local Kubernetes cluster of small services where pinned agent definitions turn inputs into artifacts through configurable workflows. Every handover is a blob with a creator and a trace ID. People decide at gates. The factory UI is the single operator surface.

## Who uses it

One engineer (today: the author) running experiments on a workstation: designing workflows, editing agents and policies as versioned prompts, submitting single inputs or whole task collections, approving or rejecting at gates, and reading what every run produced. Sessions are long, focused, and local; the browser is the control room.

## Core jobs, in order of frequency

1. Submit work to a workflow and watch its runs finish.
2. Inspect what a run produced: every artifact, who made it, in order.
3. Decide at a gate.
4. Edit a workflow graph; pick agents per step.
5. Edit an agent or policy as a new version.
6. Check the cluster and read a pod's logs.

## Principles

1. The factory is an educational tool. It doesn't need production hardening.
2. Everything configured, such as an agent or a skill, is immutable. A change creates a new version.
3. Old workflows can be rerun with the agents and skills they had. Without that capability the factory is not able to improve agents, skills, workflows and itself.
4. The factory and its components must always fail fast to surface any issues right away, so the underlying issue can be fixed.
5. There are no fallbacks.
6. The factory doesn't adapt to a model. If a model can't handle something, that is the model's problem.
7. The workflow is the source of truth. An agent gets its configuration when it is started and can't change it.
8. Every workflow leaves a trace that logs all activities, including the steps the agent takes, and references artifacts from storage, so the entire run can be analyzed. Without this we will not be able to do error analysis and drive meaningful evals.
9. Things are kept simple.
10. All services must be thread safe, deadlock free, and race condition free.
