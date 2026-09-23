package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// The OpenAI backend gates models on the application-layer client version the
// proxy presents: a model whose minimal_client_version is above codexVersion is
// hard-400ed ("requires a newer version of Codex"). Upstream keeps adding
// models to the embedded codex catalog, and until 2026-09-23 the only way that
// surfaced was somebody noticing the number during a merge. This test turns it
// into a build signal.
//
// When it fails, decide: bump the wire identity following
// docs/multi-user-pro-account-hardening.md "升版本时怎么选 UA", or, if the
// gated models are ones this deployment does not serve, raise the floor here
// deliberately with a comment saying which models were accepted as unserved.
func TestCodexVersionCoversCatalogMinimalClientVersions(t *testing.T) {
	var catalog struct {
		Models []struct {
			Slug                 string `json:"slug"`
			MinimalClientVersion string `json:"minimal_client_version"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(registry.GetCodexClientModelsJSON(), &catalog); errUnmarshal != nil {
		t.Fatalf("decode codex client models: %v", errUnmarshal)
	}
	if len(catalog.Models) == 0 {
		t.Fatal("codex client model catalog is empty")
	}

	var gated []string
	for _, model := range catalog.Models {
		minimal := strings.TrimSpace(model.MinimalClientVersion)
		if minimal == "" {
			continue
		}
		newer, errCompare := versionIsNewer(minimal, codexVersion)
		if errCompare != nil {
			t.Fatalf("model %s: %v", model.Slug, errCompare)
		}
		if newer {
			gated = append(gated, fmt.Sprintf("%s needs %s", model.Slug, minimal))
		}
	}
	if len(gated) > 0 {
		t.Fatalf("codexVersion %s is below the catalog floor for: %s", codexVersion, strings.Join(gated, ", "))
	}
}

// versionIsNewer reports whether dotted numeric version a is newer than b.
func versionIsNewer(a, b string) (bool, error) {
	fieldsA, errA := versionFields(a)
	if errA != nil {
		return false, errA
	}
	fieldsB, errB := versionFields(b)
	if errB != nil {
		return false, errB
	}
	for i := 0; i < len(fieldsA) || i < len(fieldsB); i++ {
		var valueA, valueB int
		if i < len(fieldsA) {
			valueA = fieldsA[i]
		}
		if i < len(fieldsB) {
			valueB = fieldsB[i]
		}
		if valueA != valueB {
			return valueA > valueB, nil
		}
	}
	return false, nil
}

func versionFields(version string) ([]int, error) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	fields := make([]int, 0, len(parts))
	for _, part := range parts {
		value, errParse := strconv.Atoi(part)
		if errParse != nil {
			return nil, fmt.Errorf("unparsable version %q", version)
		}
		fields = append(fields, value)
	}
	return fields, nil
}

func TestVersionIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.155.0", "0.154.0", true},
		{"0.154.0", "0.155.0", false},
		{"0.155.1", "0.155.1", false},
		{"0.155", "0.155.0", false}, // shorter version is not newer
		{"0.155.1", "0.155", true},  // trailing field breaks the tie
		{"0.16.0", "0.9.0", true},   // numeric, not lexicographic
		{"0.144.0", "0.98.0", true}, // same trap on the catalog's own floors
	}
	for _, testCase := range cases {
		got, errCompare := versionIsNewer(testCase.a, testCase.b)
		if errCompare != nil {
			t.Fatalf("versionIsNewer(%s, %s): %v", testCase.a, testCase.b, errCompare)
		}
		if got != testCase.want {
			t.Fatalf("versionIsNewer(%s, %s) = %v, want %v", testCase.a, testCase.b, got, testCase.want)
		}
	}
	if _, errCompare := versionIsNewer("0.155.x", "0.155.0"); errCompare == nil {
		t.Fatal("versionIsNewer should reject an unparsable version")
	}
}
