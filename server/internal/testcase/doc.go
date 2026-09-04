// Package testcase owns the test cases a project accumulates: the drafts a
// planner writes, the approvals a human gives them, and the archive.
//
// The executable definition lives in one jsonb `payload` column as a
// test-case@1 document, and this package never rewrites it. Approving a case
// is a status change and nothing else — the payload a run executed and the
// payload a reviewer read have to be byte-identical, or the version history
// the regression suite replays from means nothing.
package testcase
