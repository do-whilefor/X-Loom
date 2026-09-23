# P3 fixed-task evaluation

The Linux tests in `p3_*_linux_test.go` separate protocol checks from optional
real-model observations. Normal `go test ./...` runs the deterministic checks
and skips `TestP3LiveEvaluation` without contacting a model.

Run the real-model cases explicitly from the repository root:

```sh
# Supply credentials through the environment; never put them in this file.
export XLOOM_P3_LIVE=1
export ANTHROPIC_BASE_URL=https://api.stepfun.com/step_plan
export ANTHROPIC_DEFAULT_FABLE_MODEL=step-5-preview
export XLOOM_P3_OUTPUT="$PWD/.xloom/p3/baseline"
go test -count=1 -timeout 50m -v ./internal/dispatcher -run '^TestP3LiveEvaluation$'
```

`ANTHROPIC_AUTH_TOKEN` must also be set. `ANTHROPIC_MODEL`, when set, takes
precedence over `ANTHROPIC_DEFAULT_FABLE_MODEL`. Use Go's slash-separated subtest
filter to select a case, for example
`-run '^TestP3LiveEvaluation$/^P3-03_page$'`.

Defaults are 180 seconds per case, eight logical provider calls (including
compaction summaries), 8192 output tokens per call, and `max` reasoning effort.
Overrides are `XLOOM_P3_DEADLINE`, `XLOOM_P3_MAX_CALLS`,
`XLOOM_P3_MAX_TOKENS`, and `XLOOM_P3_EFFORT`. Keep these identical across
comparison runs. The provider can retry HTTP requests internally, so logical
calls and HTTP attempts are reported separately. Byte counts are not tokens;
absent usage is unknown, and reported usage is not a complete billing record.

The three discovery cases have identical root requirements and graph evidence
(63 observation facts plus two user inputs). The second relevant fact lies
beyond the maximum first page of 50 records, so increasing the page size cannot
turn the discovery condition into a single full-graph response.
They expose the second necessary fact as a full record, an ID/excerpt, or only
through graph discovery. Presentation is a controlled test ablation of a
production-generated view. These cases exercise the production prompt,
Worker, Dispatcher bridge, HTTP graph reads, version checks, and SQLite commit
using an inline v2 registered job. They do **not** measure the production
initial-view selector or an end-to-end scheduled deployment.

The twelve acceptance cases cover explicit witness acceptance, required scope,
combined requirements, identity/deployment applicability, and ledger results.
Every observation is visible initially, and each family has a sufficient
evidence control. Observations and their retained JSON are synthetic fixture
inputs. No real target is contacted or business effect proved by seeding them.
Expected outcomes and case labels are kept outside the model input.

Each local report retains initial state/view, requests and responses, graph
operations, committed state, budgets, usage, and transport attempts. Keep the
reports under ignored `.xloom/`; do not commit credentials or transcripts.
Record the tested commit and any uncommitted source changes alongside a run.

Review the completion proof and any proposed follow-up steps, not just project
status. Required fact citations and a literal discovery answer are checkable
proxies; they are not a general natural-language proof checker. A negative
case left active after a transport failure is inconclusive. An empty commit
without identifying missing work is stagnation. A new investigation can be
necessary verification or repeated work; inspect its scope and sources.
Also inspect attempted completion actions even when validation rejected them.

A single run of these fixed synthetic cases is a diagnostic baseline, not a
success rate estimate. Keep runtime changes conditional on a reproducible
failure, and rerun comparable cases after any correction.
