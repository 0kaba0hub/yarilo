## Code Review Rules

### What a review is for

Find defects the author cannot see, in this order of priority:

1. **Correctness** — wrong results, data loss, races, unhandled errors.
2. **Failure behaviour** — what happens when a dependency is slow, down, or
   returns something unexpected.
3. **Interface and contract** — public API, wire format, config keys, anything a
   caller or operator depends on and cannot change unilaterally.
4. **Tests** — does the change prove what it claims.
5. **Clarity** — naming, structure, comments that explain WHY.

Formatting and lint-enforced style are not review topics. If a machine can
decide it, let the machine decide it.

### Verify before asserting

Every finding must be traceable to code, output, or a measurement — not to
plausibility.

- Read the code path before judging it. An objection built on an assumed
  implementation wastes the author's time.
- Cite `file:line` for anything you claim the code does or fails to do.
- If you cannot verify a concern, ask it as a question and label it as such
  instead of filing it as a finding.
- When the author answers a challenge and the answer holds, say the concern is
  resolved. Do not leave it hanging.

### Write findings that can be acted on

For each finding state: what breaks, the concrete input or state that triggers
it, and where. A claim without a failure scenario is an opinion — label it a
suggestion and move on.

Rank by consequence, not by how easy the problem was to spot. A missing error
check on a hot path outranks ten naming quibbles.

Separate blocking problems from suggestions explicitly. If the change is sound,
say so plainly rather than padding the review to look thorough.

### Correctness checklist

- **Errors**: every error either handled or wrapped with context and returned.
  No silent discards. No error swallowed by a log line that then continues as if
  it succeeded.
- **Retries**: only safe when the operation is idempotent, or when it provably
  never reached the other side. A retry that can double-apply a side effect is a
  defect, not a robustness feature.
- **Concurrency**: shared state guarded or immutable; goroutine count bounded;
  the shutdown path waits for in-flight work before releasing what it uses;
  contexts honoured, not merely accepted.
- **Boundaries**: empty input, missing record, zero and negative values,
  first and last element, and the case where a remote peer disappears mid-call.
- **Blast radius**: when a change makes a resource shared, ask what one failure
  now costs and whether recovery is faster than the timeout that treats the
  dependents as dead.

### Performance claims

- Require a measurement, not an argument. State the before and after, and the
  configuration used.
- One variable per experiment. Two changes at once prove nothing.
- Wall-clock under contention measures queueing, not cost. A number taken from a
  saturated system does not establish where the work went.
- Averaged metrics hide short bursts; check the resolution is finer than the
  event being explained.
- Ask for the residual: what fraction of the problem is left, and what is it.

### Tests

- Cover the failure the change fixes. A fix without a test that would have
  caught it is incomplete.
- A regression test must fail on the pre-change code by construction — not
  incidentally.
- Assert the exact property. A weaker assertion that also passes for the broken
  behaviour is worse than no test, because it looks like coverage.
- Deterministic: no sleeps standing in for synchronisation, no dependence on
  wall-clock timing or execution order.
- If a guarantee cannot be exercised where it runs, test it at the level where it
  can be.
- A mutation described in a PR body is a claim. A mutation that is proven quotes
  its red message verbatim. Ask for the output, not the account of it: a trap
  that watched a third of what it named was approved on a description (#1652).

### Interfaces and compatibility

- Public signatures, wire formats, config keys and metric labels are contracts.
  Renaming or repurposing one silently breaks a consumer that cannot be changed
  in the same commit.
- New enumerated values must come from a bounded set. Unbounded label or key
  cardinality is a defect.
- A behavioural constant an operator cannot change is a finding: it belongs in
  configuration.
- Removing or defaulting a configuration value: check how the framework merges
  defaults, so "omitted" and "unset" mean what the author intended.

### Observability

- A new failure mode needs a way to see it. If the change can fail invisibly,
  that is a finding.
- Metric and log semantics must be documented where they are surprising — a
  latency measurement that includes deliberate delays will be misread otherwise.
- Distinguish outcomes that call for different responses. Collapsing them into
  one counter loses exactly the information an incident needs.

### Security and data handling

- Never print or log credentials, tokens, or personal data — and never ask the
  author to paste them into the review. Assert on presence and shape instead.
- Untrusted input validated at the boundary, before it reaches anything that
  trusts it.
- Errors returned to a remote caller must not leak internal detail that the
  caller should not distinguish.

### Comments and documentation

- Comments explain WHY: a constraint, an invariant, a compatibility quirk, a
  decision that looks wrong without context. A comment restating the code is
  noise; ask for its removal.
- A non-obvious decision with no recorded reason is a finding — the next reader
  will otherwise "fix" it.
- Documentation that the change makes wrong must be updated in the same change.
- Comment size: 1–2 lines is the norm. A block of 3+ lines in a diff is a
  finding — the explanation belongs in the documentation repo (or docs-internal
  for wire formats), with at most a one-line pointer left in code.
- No history or meta-narrative in comments ("previously…", "changed because…");
  history lives in issues, referenced as `(#NNNN)`.
- Removing a comment is a finding only if an invariant vanished with it — check
  what the comment protected, not that a line disappeared.
- Comment volume: a package trending toward the 10% ceiling (comment lines vs Go
  lines, see the comment-count tool and #1620) is worth flagging before the gate
  does.
