# Aspect Development Guidelines

When developing in this repository or implementing Aspect changes:

1. **Spec is the Contract**:
   - The Aspect specification (`_aspect/aspect.yaml` and module briefs) governs behavior.
   - Any change to behavior must be reflected in the spec; undocumented behavior is treated as spec drift.

2. **Separation of Author and Judge**:
   - Orchestration across the lifecycle is directed by the lead orchestrator (`aspect_developer`), who coordinates specialized agents rather than writing code directly.
   - High-level component structure and user journey steps are formulated by the architect (`aspect_architect`).
   - Detailed blueprint and invariant planning is performed by the design sub-agent (`aspect_designer`).
   - Implementation and test coding is performed collaboratively by the developer sub-agent (`aspect_coder`) and test designer sub-agent (`aspect_test_designer`).
   - Plan conformance and completeness is audited by the plan director (`aspect_plan_director`), verifying the plan has been followed to the dot and all steps are concluded and tested.
   - Independent validation is performed by dedicated review subagents (`test_evaluator`, `adversarial_reviewer`, `spec_keeper`, `quality_reviewer`, `security_reviewer`, `test_adequacy_reviewer`).
   - An author never reviews their own work; validation agents evaluate the change objectively.

3. **Mandatory Verification**:
   Before marking any change complete, run:
   ```bash
   gofmt -l . && go vet ./... && go test -race ./...
   ```
   If an Aspect spec exists:
   ```bash
   go run ./cmd/aspect validate _aspect/aspect.yaml
   go run ./cmd/aspect gate plan
   ```

4. **Zero-Defect & Comprehensive Test Gate Clearance**:
   - All validation subagents including `test_evaluator` and `aspect_plan_director` must return clean verdicts.
   - Functional tests must be present, unit coverage adequate, and tests efficient and regression-proof.
   - Any critical or high finding must be remediated with automated regression tests before the task is considered done.
