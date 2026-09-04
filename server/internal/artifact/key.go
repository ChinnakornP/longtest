package artifact

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// KeyPrefix returns the only prefix a run's daemon may write below:
//
//	orgs/{orgID}/runs/{YYYY-MM-DD}/{runID}/
//
// The date segment is the run's creation day in UTC, so a run that starts
// before midnight and finishes after it keeps all of its evidence together.
func KeyPrefix(orgID, runID uuid.UUID, day time.Time) string {
	return fmt.Sprintf("orgs/%s/runs/%s/%s/", orgID, day.UTC().Format(time.DateOnly), runID)
}

// segmentRe is the one tail segment shape both this package and the
// artifacts_storage_key_layout CHECK accept. It must start with an
// alphanumeric, which is what drops "..", "." and dotfiles: S3 keys are opaque,
// but these keys are joined onto filesystem paths downstream (report bundling,
// artifact download) and a ".." segment there is a traversal.
var segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// ObjectKey builds the storage key for one piece of evidence:
//
//	orgs/{orgID}/runs/{YYYY-MM-DD}/{runID}/{testCaseRef}/{name}
//
// testCaseRef may be empty for a run-level artifact such as the discovery HAR,
// in which case that segment is omitted. It is a test case *ref* ("TC-001"),
// not this database's uuid: the daemon composes the key from the test-case
// document it was handed over the control plane, which never carries the uuid.
//
// Every segment is validated rather than escaped. A name can be derived from
// something the daemon read off the page under test, so a separator or a
// traversal segment is rejected outright instead of being allowed to reach
// outside the run's prefix.
func ObjectKey(orgID, runID uuid.UUID, day time.Time, testCaseRef, name string) (string, error) {
	if err := validateSegment("name", name); err != nil {
		return "", err
	}
	prefix := KeyPrefix(orgID, runID, day)
	if testCaseRef == "" {
		return prefix + name, nil
	}
	if err := validateSegment("testCaseRef", testCaseRef); err != nil {
		return "", err
	}
	return prefix + testCaseRef + "/" + name, nil
}

func validateSegment(label, segment string) error {
	if !segmentRe.MatchString(segment) {
		return fmt.Errorf(
			"%s must be 1-200 characters of letters, digits, dot, dash or underscore and start with a letter or digit",
			label)
	}
	return nil
}

// CheckKeyUnderPrefix reports whether key is a well-formed object key inside
// prefix.
//
// This is the gate PutURL runs before it signs anything, and it is deliberately
// structural rather than a strings.HasPrefix: a key of
// "orgs/{org}/runs/{day}/{run}/../../other-org/x" has the right prefix and is
// still an escape once the key is used as a path.
func CheckKeyUnderPrefix(prefix, key string) error {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return fmt.Errorf("key is not under this run's prefix %q", prefix)
	}
	segments := strings.Split(rest, "/")
	if len(segments) > 2 {
		return fmt.Errorf("key has %d segments below the run prefix, at most 2 are allowed", len(segments))
	}
	for _, segment := range segments {
		if err := validateSegment("key segment", segment); err != nil {
			return err
		}
	}
	return nil
}
