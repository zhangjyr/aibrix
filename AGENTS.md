# AIBrix

AIBrix is a Kubernetes-native platform for building scalable GenAI inference
infrastructure. This repository contains Go controllers, gateway plugins,
custom resources, deployment manifests, tests, and Python runtime components.
This file captures repository-specific guidance for AI coding agents working in
the repository root. Keep it concise, factual, and aligned with the current
codebase.

Rules in a deeper `AGENTS.md` take precedence for files under that directory.
For work inside `python/aibrix`, follow `python/aibrix/AGENTS.md`; that file is
the source of truth for Python-specific setup, validation, architecture, and
conventions.

## Document ownership

- `AGENTS.md` is the source of truth for repo-wide workflow, completion,
  verification, and cross-language collaboration rules.
- `python/aibrix/AGENTS.md` is the source of truth for Python-runtime setup,
  validation commands, subsystem guidance, and Python-specific development
  rules.
- When present, `docs/superpowers/agent-workflow.md` is the detailed source of
  truth for repo-level and Go-root superpowers spec and plan workflow.
- Keep repo workflow rules here. Do not duplicate the full Python operational
  guide in the root file.
- When a rule changes, update the owning file directly instead of creating a
  second source of truth in README text, ad hoc notes, or copied prompts.

## Scope and repository layout

| Path | Role |
|------|------|
| `api/` | Kubernetes API types and CRD definitions |
| `pkg/controller/` | Kubernetes controllers and reconciliation logic |
| `pkg/plugins/` | Gateway plugins and request processing |
| `pkg/` | Shared Go libraries, clients, caches, metrics, and utilities |
| `cmd/` | Go binary entrypoints |
| `config/` | Kustomize configuration, CRDs, RBAC, and deployment manifests |
| `benchmarks/` | Benchmarking tools and workloads |
| `development/` | Local development assets and guides |
| `deployment/` | Deployment assets and configuration |
| `test/integration/` | Ginkgo-based integration tests |
| `test/e2e/` | Kind and cluster-level end-to-end tests |
| `hack/` | Code generation, verification, and CI scripts |
| `python/aibrix/` | Python runtime, metadata service, downloader, batch, and GPU optimizer |
| `python/aibrix_kvcache/` | Python distributed KV-cache components |
| `apps/` | Application services and the console |
| `docs/` | Project documentation and generated documentation assets |
| `samples/` | Example deployments and feature samples |

## Architecture pointers

- `pkg/controller/` implements Kubernetes controllers such as PodAutoscaler,
  ModelAdapter, KVCache, RayCluster orchestration, StormService, and RoleSet.
- `pkg/plugins/gateway/` implements request routing, queueing, and rate
  limiting for the gateway path.
- `cmd/` contains the main Go entry points for controllers and plugins.
- `config/` contains CRDs, RBAC, Kustomize bases, overlays, and deployment
  manifests.

## Working rules

- State the intended scope and success criteria before making a non-trivial
  change.
- Re-open the relevant files, tests, and nearby docs before editing. Do not
  rely on stale session context.
- Read the closest existing implementation and its tests before introducing a
  new pattern or abstraction.
- For behavior changes, bug fixes, refactors, or other multi-step work,
  clarify the approach before implementation and write a short plan when the
  task is non-trivial.
- For repo-level or Go-root new features, behavior changes, architecture
  changes, major refactors, or other non-trivial multi-step work, follow
  `docs/superpowers/agent-workflow.md` only when that file is available.
- When that workflow is available and requires a formal design, write it under
  `docs/superpowers/specs/`.
- When that workflow is available and requires a formal implementation plan,
  write it under `docs/superpowers/plans/`.
- Prefer adding or updating focused tests before implementation when practical,
  especially when behavior changes.
- Keep changes focused. Do not combine unrelated refactors, formatting, or
  generated output with a feature or bug fix.
- Keep changes localized to the subsystem you touch unless shared
  infrastructure must move together.
- Verify behavior from code and tests rather than inferring it from filenames.
- Preserve unrelated user changes in the working tree.
- Do not push branches, rewrite history, modify remote GitHub state, or send
  external messages without explicit authorization for that action.
- Ask before upgrading dependencies or changing `.github/` workflow policy.
- Use existing AIBrix tooling for validation. Do not invent replacement lint,
  format, or test workflows when repo commands already exist.

## API and compatibility surfaces

Treat the following as compatibility surfaces:

- Kubernetes API types, CRD schemas, JSON/YAML tags, defaults, validation,
  list semantics, subresources, and printer columns.
- Public HTTP endpoints, request and response fields, headers, status codes,
  and gateway plugin contracts.
- CLI flags, configuration keys, environment variables, labels, and
  annotations used by deployment or controller logic.

Do not rename, remove, or repurpose an existing field or key without an
explicit migration and compatibility plan. Preserve pointer and `omitempty`
choices when absent, zero, and explicit values have different meanings.
Keep status fields observational; status transitions must remain consistent
with every controller that writes or consumes them.

Keep API types declarative. Admission behavior belongs in webhooks,
reconciliation policy belongs in controllers, and transport compatibility
belongs in the relevant API or gateway layer.

## Build and verification

Run commands from the repository root. Start with the narrowest relevant check
and run broader checks for changes that cross package or language boundaries.

| Command | Purpose |
|---------|---------|
| `make build` | Build Go binaries |
| `make fmt` | Format Go code |
| `make vet` | Run `go vet ./...` |
| `make generate` | Regenerate Go and Kubernetes generated artifacts |
| `make manifests` | Regenerate webhook, RBAC, and CRD manifests |
| `make verify` | Verify generated code and CRD synchronization |
| `make lint` | Run Go linting |
| `make lint-all` | Run license and Go lint checks |
| `make test` | Run Go unit tests, excluding integration and E2E packages |
| `make test-race-condition` | Run Go unit tests with the race detector |
| `make test-integration` | Run integration tests |
| `make python-ci` | Run Python Ruff, mypy, and tests for `python/aibrix` |
| `make test-e2e` | Run Kind-based end-to-end tests |
| `make docker-build-all` | Build all container images |
| `make docker-build-controller-manager` | Build the controller-manager image |
| `make docker-build-gateway-plugins` | Build the gateway plugins image |
| `make docker-build-runtime` | Build the runtime image |
| `make docker-build-metadata-service` | Build the metadata service image |

For a Go package, a focused check such as `go test ./pkg/<package>/...` is
appropriate while iterating. For cluster-level behavior, use the existing
helpers and test organization under `test/e2e/`; do not replace polling with
arbitrary sleeps.

When API types, controller-gen markers, or CRDs change, run the applicable
generation and verification targets and include the resulting generated files
in the change. Do not hand-edit generated files unless the generator explicitly
requires it.

Do not claim a change is complete until you have run the relevant existing
validation commands and reviewed the results. Verification claims must be backed
by fresh command output, not memory or assumptions. Missing validation evidence
means the task is not complete.

## Coding conventions

- Follow existing Go package structure and standard Go style; use `gofmt` or
  `make fmt`.
- Put reusable implementation in `pkg/`; keep `cmd/` entrypoints thin.
- Follow existing controller-runtime and Kubernetes API patterns before adding
  new abstractions in Go code.
- Use table-driven tests where they match the surrounding package style.
- Add or update tests for behavior changes, including failure and compatibility
  cases when applicable.
- Comments should explain non-obvious reasons or invariants, not restate code.
- Preserve existing logging, error handling, retry, timeout, and context
  propagation patterns unless the change specifically addresses them.
- Keep Go-root validation aligned with the most relevant existing `make` or Go
  test targets.
- If `docs/superpowers/` is available, keep repo-level and Go-root superpowers
  artifacts there. Do not place Python-runtime design or execution-plan docs
  there.
- For Python changes, follow the more specific rules in
  [`python/aibrix/AGENTS.md`](python/aibrix/AGENTS.md) and run checks from that
  subtree as documented there. Do not duplicate Python guidance here.

## Multi-agent context

- When using a subagent or reviewer, provide the repository root path, the
  exact task scope, and the relevant files or documents.
- Keep subagent tasks independent when running in parallel. Do not split work
  with shared write conflicts or hidden ordering dependencies.
- Do not accept a pass or fail conclusion that cites files outside the intended
  task scope unless those files were explicitly included.
- If review feedback is based on the wrong files, wrong directory, or wrong
  scope, treat it as invalid and restate the context before proceeding.

## Pull requests and documentation

- Keep a PR focused on one issue or coherent behavior change.
- Use the repository [PR template](.github/PULL_REQUEST_TEMPLATE.md), link the
  relevant issue, and describe the tests actually run.
- Update user or developer documentation when behavior, configuration, API,
  deployment, or CLI usage changes.
- Large design changes should be discussed before implementation and recorded
  in the appropriate documentation area.
- Review every generated or AI-assisted change yourself; do not submit code
  that you cannot explain and verify.
- For larger, riskier, or cross-subsystem changes, perform explicit review or a
  careful self-check before closing the task.

## Do not do this

- Do not introduce repo-wide workflow rules in multiple files with conflicting
  wording.
- Do not claim a task is done without naming the validation that was run.
- Do not replace existing AIBrix lint, format, or test entrypoints with ad hoc
  one-off commands in documentation or handoff notes.
- Do not place Python-runtime superpowers specs or plans under
  `docs/superpowers/` when that directory exists; keep them under
  `python/aibrix/superpowers/`.
- Do not let subagent or reviewer conclusions drift beyond the scoped task.

## Codebase freshness rule

Do not assume the codebase is unchanged between tasks, even within the same
session.

Before making edits or reasoning about behavior:

- Re-open the relevant files to confirm their current contents and surrounding
  context.
- Re-verify key call sites, signatures, and invariants through code reading or
  repository history.
- Use Git tools such as diff, blame, log, or show to confirm what changed since
  the last task and to validate any assumptions that depend on prior state.

## Further reading

- [Contributing guide](CONTRIBUTING.md)
- [Development guide](development/README.md)
- [Test guide](test/README.md)
- [Project documentation](docs/README.md)
