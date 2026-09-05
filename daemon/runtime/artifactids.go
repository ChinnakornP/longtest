package runtime

import (
	"fmt"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Artifact ids have to be unique across a run, and the executor cannot make
// them so.
//
// execution-result@1 says an ArtifactId is a "run-local handle the daemon makes
// up", and its $comment spells out the invariant: unique within one run, and a
// duplicate must be rejected rather than resolved last-write-wins. The executor
// mints them from a counter that starts at zero for every test case, so a run of
// forty cases produces forty artifacts called `screenshot-0`. Ingest builds one
// run-wide handle -> uuid map from run.result.artifacts[], so the last case
// ingested wins and a finding about case 3 that cites `screenshot-0` links the
// screenshot from case 40. The report renders it, the link opens, and the
// picture is of a different test.
//
// The daemon is where the contract puts this responsibility, and it is the only
// place with a run-wide view: the executor is invoked per case and cannot know
// what the others produced. So the ids are namespaced here, once, before the
// result is kept — every reference inside the same document moving with them.
//
// The counterpart hardening on the server — rejecting a duplicate handle at
// ingest instead of overwriting the map entry — is deliberately not in this
// change: an older daemon would start failing runs the moment it landed, and
// expand-then-contract means the producer moves first.

// namespaceArtifactIDs rewrites one execution's artifact ids so they cannot
// collide with another execution's, and repoints every reference to them.
//
// ordinal is the execution's position in the run, which is stable for a given
// assignment and short enough to keep the ids inside the contract's 64
// characters.
func namespaceArtifactIDs(result *qaschema.ExecutionResult, ordinal int) {
	if len(result.Artifacts) == 0 {
		return
	}

	renamed := make(map[string]string, len(result.Artifacts))
	for i := range result.Artifacts {
		old := result.Artifacts[i].ID
		next := fmt.Sprintf("e%d-%s", ordinal, old)
		if len(next) > 64 {
			// Unreachable with executor-minted ids (`kind-n`), and a silent
			// truncation here would reintroduce exactly the collision this
			// function exists to remove. Leaving the id alone keeps the
			// document valid; the duplicate it may cause is the status quo.
			continue
		}
		renamed[old] = next
		result.Artifacts[i].ID = next
	}

	for i := range result.Steps {
		result.Steps[i].ArtifactIDs = rename(result.Steps[i].ArtifactIDs, renamed)
	}
	for i := range result.Assertions {
		result.Assertions[i].ArtifactIDs = rename(result.Assertions[i].ArtifactIDs, renamed)
	}
}

// rename maps a reference list through the rename table, leaving anything the
// table does not know untouched — an id that names no artifact of this
// execution is a problem to report, not one to silently drop.
func rename(ids []qaschema.ArtifactID, renamed map[string]string) []qaschema.ArtifactID {
	for i, id := range ids {
		if next, ok := renamed[id]; ok {
			ids[i] = next
		}
	}
	return ids
}
