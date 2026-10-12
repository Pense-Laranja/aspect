# Aspect Development Guidelines

When developing in this repository or implementing Aspect changes:

1. **Spec is the Contract**:
   - The Aspect specification (`_aspect/aspect.yaml` and module briefs) governs behavior.
   - Any change to behavior must be reflected in the spec; undocumented behavior is treated as spec drift.

2. **Separation of Author and Judge**:
   - Planning is performed by the design sub-agent (`aspect_designer`).
   - Implementation and test coding is performed collaboratively by the developer sub-agent (`aspect_coder`) and test designer sub-agent (`aspect_test_designer`).
   - Validation is performed by dedicated review subagents (`test_evaluator`, `adversarial_reviewer`, `spec_keeper`, `quality_reviewer`, `security_reviewer`, `test_adequacy_reviewer`).
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
   - All validation subagents including `test_evaluator` must return clean verdicts.
   - Functional tests must be present, unit coverage adequate, and tests efficient and regression-proof.
   - Any critical or high finding must be remediated with automated regression tests before the task is considered done.
