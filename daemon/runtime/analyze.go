package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ChinnakornP/longtest/daemon/analysis"
	"github.com/ChinnakornP/longtest/daemon/artifacts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// The analysis phase: failed executions in, findings out.
//
// Four steps, and the model is only the third of them:
//
//  1. Collect the evidence deterministically, one bundle per failed execution,
//     and upload each bundle as an artifact so the finding that cites it points
//     at something a person can open.
//  2. Run the rule pass. Network, auth and timeout failures are decided here
//     and never reach a model. A run whose every failure is one of those never
//     starts an AI CLI at all, which is the cheap case and also the common one
//     when an environment is broken.
//  3. Ask the model about what is left, with [analysis.Context.ReviewHook]
//     wired into the task so a fabricated citation becomes the next attempt's
//     feedback rather than a stored finding.
//  4. Guarantee the invariant the model cannot be trusted with: every failed
//     execution leaves this function with exactly one finding.
//
// Step 4 is what makes a failed analyst survivable. An analysis that cannot be
// completed still fails the phase — an answer that never satisfied its contract
// is a run error and saying otherwise would hide a broken prompt for months —
// but the findings are built and returned alongside the error, and the phase
// failure path in run.go keeps them. So the report a person opens after a
// failed analysis still says something about every red row, rather than
// carrying the executions with nothing attached to them, which is the "23 tests
// failed and nothing says why" outcome arrived at from the other direction.

// evidenceFileName is what the per-execution evidence bundle is called, in the
// workspace and in object storage.
const evidenceFileName = "analysis-evidence.json"

// analyse runs the whole phase and returns the findings for run.result.
func (rc *runController) analyse(
	ctx context.Context,
	ws *workspace.Workspace,
	ph phase,
	executions []qaschema.ExecutionResult,
	testCases []qaschema.TestCase,
	appMap *qaschema.ApplicationMap,
	uploader *artifacts.Uploader,
) ([]json.RawMessage, []qaschema.Artifact, error) {
	collector := analysis.Collector{
		ArtifactDir: func(ref string) (string, error) { return ws.Path(workspace.PhaseExecution, ref) },
		AppMap:      appMap,
		Logger:      rc.logger,
	}
	bundles := collector.Collect(executions, testCases)
	if len(bundles) == 0 {
		rc.emit(qaschema.RunEventPayloadLevelInfo, "analysis_skipped",
			"every execution passed, so there is nothing to explain", nil)
		return nil, nil, nil
	}

	// The bundle becomes an artifact before anything is classified. Two things
	// depend on it: a rule verdict needs an artifact id to cite, and an
	// execution that failed before it captured a screenshot has no other one —
	// a transport failure produces no evidence files at all, and finding@1
	// requires at least one citation. Uploading what the analyst read is also
	// the only way a reader can check a verdict rather than take it.
	evidence, err := rc.uploadEvidenceBundles(ctx, ws, bundles, uploader)
	if err != nil {
		return nil, nil, err
	}
	attachEvidenceArtifacts(bundles, evidence)

	decided, ambiguous := analysis.Partition(bundles)
	rc.narrateRules(decided, ambiguous)

	documents, err := analysis.EncodeAll(decided)
	if err != nil {
		return nil, evidence, failure(qaschema.RunErrorCodeInternal, err, "could not encode a rule-pass finding")
	}

	// analystErr is held rather than returned: the phase still fails on it, and
	// it still has to fail after the gap-filling below, or a failed analyst
	// would take the findings for the failures it DID classify down with it.
	var analystErr error
	reason := "no analysis was needed"
	if len(ambiguous) > 0 {
		modelFindings, err := rc.askAnalyst(ctx, ws, ph, ambiguous, testCases)
		switch {
		case err != nil && ctx.Err() != nil:
			return documents, evidence, err
		case err != nil:
			analystErr = err
			reason = err.Error()
			rc.emit(qaschema.RunEventPayloadLevelError, "analysis_unavailable",
				"the failure analyst produced no usable verdict; the executions it could not classify are recorded as UNKNOWN",
				map[string]any{"error": reason, "executions": len(ambiguous)})
		default:
			floored, downgraded, floorErr := analysis.ApplyConfidenceFloor(modelFindings, analysis.MinConfidence)
			if floorErr != nil {
				return documents, evidence, failure(qaschema.RunErrorCodeAgentOutputInvalid, floorErr,
					"could not apply the confidence floor to the analysis result")
			}
			if len(downgraded) > 0 {
				sort.Strings(downgraded)
				rc.emit(qaschema.RunEventPayloadLevelWarn, "finding_downgraded",
					fmt.Sprintf("%d finding(s) were recorded as UNKNOWN: the analyst's own confidence was below %.2f",
						len(downgraded), analysis.MinConfidence),
					map[string]any{"testCases": downgraded, "floor": analysis.MinConfidence})
			}
			documents = append(documents, floored...)
			reason = "the analyst returned no finding for it"
		}
	}

	// Unconditional, and before the analyst's error is returned: whatever
	// happened above, a failed execution leaves here with a finding.
	documents, filled, err := analysis.CoverGaps(documents, bundles, reason)
	if err != nil {
		return documents, evidence, failure(qaschema.RunErrorCodeInternal, err, "could not complete the findings")
	}
	if len(filled) > 0 {
		sort.Strings(filled)
		rc.emit(qaschema.RunEventPayloadLevelWarn, "finding_synthesised",
			fmt.Sprintf("%d failed execution(s) had no finding and were recorded as UNKNOWN", len(filled)),
			map[string]any{"testCases": filled})
	}

	if analystErr != nil {
		return documents, evidence, analystErr
	}

	rc.emit(qaschema.RunEventPayloadLevelInfo, "analysis_finished",
		fmt.Sprintf("%d finding(s) for %d failed execution(s)", len(documents), len(bundles)),
		map[string]any{"findings": len(documents), "failures": len(bundles), "byRule": len(decided)})
	return documents, evidence, nil
}

// askAnalyst runs the AI phase over the bundles the rules could not decide.
func (rc *runController) askAnalyst(
	ctx context.Context,
	ws *workspace.Workspace,
	ph phase,
	bundles []analysis.Bundle,
	testCases []qaschema.TestCase,
) ([]json.RawMessage, error) {
	inputs, err := analysisInputs(bundles, testCases)
	if err != nil {
		return nil, failure(qaschema.RunErrorCodeInternal, err, "could not write the analysis inputs")
	}

	// The gate travels with the task rather than running after it, so a
	// fabricated citation becomes the next attempt's feedback instead of the
	// phase's cause of death. See daemon/analysis/review.go.
	review := analysis.NewContext(bundles).ReviewHook()

	raw, err := rc.runAgent(ctx, ws, ph, findingSchemaID, inputs, review)
	if err != nil {
		return nil, err
	}

	// finding@1 describes ONE finding and the analyst produces one per failed
	// execution, so out.json is an array whose elements are each validated on
	// their own. The elements stay as the bytes that were validated: see
	// resultPayload on why re-encoding one through the generated struct turns
	// a valid document into an invalid one.
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, failure(qaschema.RunErrorCodeAgentOutputInvalid, err,
			"the analyst was asked for an array of %s documents", findingSchemaID)
	}
	for i, element := range elements {
		if err := rc.validateAgainst(findingSchemaID, element, ph); err != nil {
			return nil, failure(qaschema.RunErrorCodeAgentOutputInvalid, err, "analysis output item %d is invalid", i)
		}
	}
	return elements, nil
}

// findingSchemaID is the contract the analysis phase answers in.
const findingSchemaID = "finding@1"

// analysisInputs are the files placed in the analysis workspace.
//
// One file per failed execution rather than one big document: the model is
// asked about each in turn, and a per-case file is what it can cite by name.
// The test cases go in whole because a TEST_BUG verdict is a claim about what
// the case asked for, which cannot be checked against the execution alone.
func analysisInputs(bundles []analysis.Bundle, testCases []qaschema.TestCase) (map[string][]byte, error) {
	inputs := make(map[string][]byte, len(bundles)+2)
	index := make([]string, 0, len(bundles))

	for _, b := range bundles {
		data, err := b.Encode()
		if err != nil {
			return nil, err
		}
		name := evidenceInputName(b.TestCaseRef)
		inputs[name] = data
		index = append(index, name)
	}

	cases, err := json.MarshalIndent(testCases, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runtime: encode test-cases.json: %w", err)
	}
	inputs["test-cases.json"] = cases

	sort.Strings(index)
	manifest, err := json.MarshalIndent(map[string]any{
		"failedExecutions": len(bundles),
		"evidenceFiles":    index,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runtime: encode failures.json: %w", err)
	}
	inputs["failures.json"] = manifest
	return inputs, nil
}

// evidenceInputName is the workspace file name for one bundle. The ref is a
// test-case@1 id, whose pattern is already narrower than a file name needs to
// be, so it goes in as-is.
func evidenceInputName(ref string) string { return "evidence-" + ref + ".json" }

// uploadEvidenceBundles writes each bundle into the analysis workspace and puts
// it in object storage.
//
// A bundle that cannot be uploaded is a warning rather than a failure: the
// execution still has its own artifacts to cite in the usual case, and losing
// the whole report because one JSON file did not reach S3 is the wrong trade.
// The one case it costs something is an execution with no other evidence, and
// [analysis.CoverGaps] reports that in as many words rather than silently.
func (rc *runController) uploadEvidenceBundles(
	ctx context.Context,
	ws *workspace.Workspace,
	bundles []analysis.Bundle,
	uploader *artifacts.Uploader,
) ([]qaschema.Artifact, error) {
	out := make([]qaschema.Artifact, 0, len(bundles))
	for i, b := range bundles {
		data, err := b.Encode()
		if err != nil {
			return out, failure(qaschema.RunErrorCodeInternal, err, "could not encode the evidence bundle")
		}
		dir, err := ws.MkdirAll(workspace.PhaseAnalysis, b.TestCaseRef)
		if err != nil {
			return out, failure(qaschema.RunErrorCodeInternal, err,
				"could not create the analysis directory for %s", b.TestCaseRef)
		}
		// Written straight into the directory MkdirAll just validated and
		// created, the way the executor writes into its own artifact
		// directory: the workspace's WriteFile takes a single file name and
		// this one lives a directory down, under the case's ref.
		local := filepath.Join(dir, evidenceFileName)
		if err := os.WriteFile(local, data, 0o600); err != nil {
			return out, failure(qaschema.RunErrorCodeInternal, err,
				"could not write the evidence bundle for %s", b.TestCaseRef)
		}

		key, err := artifacts.KeyUnder(uploader.Prefix(), b.TestCaseRef, evidenceFileName)
		if err != nil {
			rc.logger.Warn("could not build an evidence bundle key", "testCaseRef", b.TestCaseRef, "error", err)
			continue
		}
		stored, err := uploader.Upload(ctx, artifacts.Upload{
			Key:  key,
			Path: local,
			Kind: qaschema.ArtifactKindReport,
			// Cannot collide with an executor-minted id: those are namespaced
			// `e{n}-` by this point (artifactids.go) and were `kind-n` before.
			ID:          fmt.Sprintf("analysis-%d", i),
			ContentType: "application/json",
		})
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			rc.logger.Warn("could not upload an evidence bundle", "testCaseRef", b.TestCaseRef, "error", err)
			continue
		}
		out = append(out, stored)
	}
	return out, nil
}

// attachEvidenceArtifacts adds each uploaded bundle to the bundle it describes,
// so it is citable by the rules, offerable to the model and accepted by the
// review gate — which reads its artifact set from these same lists.
func attachEvidenceArtifacts(bundles []analysis.Bundle, uploaded []qaschema.Artifact) {
	byRef := make(map[string]qaschema.Artifact, len(uploaded))
	for _, artifact := range uploaded {
		byRef[testCaseRefOfKey(artifact.Key)] = artifact
	}
	for i := range bundles {
		if artifact, ok := byRef[bundles[i].TestCaseRef]; ok {
			bundles[i].Artifacts = append(bundles[i].Artifacts, artifact)
		}
	}
}

// testCaseRefOfKey reads the case segment back out of an object key:
// orgs/{orgId}/runs/{day}/{runId}/{testCaseRef}/{name}.
func testCaseRefOfKey(key string) string {
	dir := filepath.ToSlash(filepath.Dir(key))
	if i := len(dir) - 1; i >= 0 {
		for j := i; j >= 0; j-- {
			if dir[j] == '/' {
				return dir[j+1:]
			}
		}
	}
	return dir
}

// narrateRules puts the split between the rule pass and the model on the run's
// event stream.
//
// Counts and class names only — no model output and no page content — so it can
// go to the backend without passing the untrusted boundary again. It is also
// the line an operator reads to see that a run classified forty failures
// without paying for a single token.
func (rc *runController) narrateRules(decided []analysis.Verdict, ambiguous []analysis.Bundle) {
	if len(decided) == 0 {
		rc.emit(qaschema.RunEventPayloadLevelInfo, "analysis_rules_finished",
			fmt.Sprintf("no failure was decidable by rule; %d go to the analyst", len(ambiguous)),
			map[string]any{"byRule": 0, "byModel": len(ambiguous)})
		return
	}

	byClass := map[string]int{}
	byRule := map[string]int{}
	for _, verdict := range decided {
		byClass[string(verdict.FailureClass)]++
		byRule[verdict.Rule]++
	}
	rc.emit(qaschema.RunEventPayloadLevelInfo, "analysis_rules_finished",
		fmt.Sprintf("%d failure(s) classified by rule, %d go to the analyst", len(decided), len(ambiguous)),
		map[string]any{
			"byRule":   len(decided),
			"byModel":  len(ambiguous),
			"byClass":  byClass,
			"rules":    byRule,
			"noAgent":  len(ambiguous) == 0,
			"failures": len(decided) + len(ambiguous),
		})
}
