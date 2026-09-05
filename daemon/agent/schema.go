package agent

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// ContractDir is where the runner places the schema documents a phase's answer
// is validated against.
//
// They are files rather than prompt text for the same reason every other input
// is (ADR-003), and they are placed at all because the alternative does not
// work: a prompt that names a contract without showing it is asking the model
// to guess property names, and a model guessing "selector" where the contract
// says "locators[].kind" fails validation three times and burns the run.
const ContractDir = "contract"

// contractFile is the schema id as a file name. The `@` in "test-plan@1" is
// legal in a path but reads as an e-mail address to half the tools an operator
// might open the workspace with.
func contractFile(schemaID string) string {
	return path.Join(ContractDir, strings.ReplaceAll(schemaID, "@", "-v")+".json")
}

// writeContracts places the named contract and everything it references.
//
// The references matter: test-plan@1 is mostly a $ref to test-case@1, and a
// model handed only the outer document would see an array of "something" and
// invent the step vocabulary.
func writeContracts(ws *security.Workspace, schemaID string) (string, error) {
	if err := ws.MkdirAll(ContractDir); err != nil {
		return "", fmt.Errorf("agent: create %s: %w", ContractDir, err)
	}

	byURI := map[string]string{}
	for _, id := range qaschema.SchemaIDs {
		if uri, ok := qaschema.SchemaURI(id); ok {
			byURI[uri] = id
		}
	}

	written := map[string]bool{}
	var place func(id string) error
	place = func(id string) error {
		if written[id] {
			return nil
		}
		written[id] = true

		document, err := qaschema.SchemaDocument(id)
		if err != nil {
			return fmt.Errorf("agent: load contract %s: %w", id, err)
		}
		// Re-encoded rather than copied out of the embedded filesystem: the
		// package exposes the parsed document, and a schema that round-trips
		// through the same decoder the validator uses cannot disagree with it.
		raw, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return fmt.Errorf("agent: encode contract %s: %w", id, err)
		}
		if err := ws.WriteFile(contractFile(id), append(raw, '\n')); err != nil {
			return fmt.Errorf("agent: write contract %s: %w", id, err)
		}

		for _, uri := range refs(document) {
			if referenced, ok := byURI[uri]; ok {
				if err := place(referenced); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := place(schemaID); err != nil {
		return "", err
	}
	return contractFile(schemaID), nil
}

// refs walks a decoded schema for every external $ref target, stripping any
// fragment so "…/test-case/1#/$defs/step" resolves to the document that
// defines it.
func refs(document any) []string {
	var found []string
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if ref, ok := typed["$ref"].(string); ok {
				if uri, _, _ := strings.Cut(ref, "#"); uri != "" {
					found = append(found, uri)
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
	return found
}
