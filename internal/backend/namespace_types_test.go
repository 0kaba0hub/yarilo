package backend

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// The validator and the builder must accept exactly the same types.
//
// They disagreed on one value: an absent type passed validation and was then
// dropped by the builder with a log warning — which is the failure #1086 is
// about, kept alive for the field that is easiest to forget. Testing each side
// on its own would not have found it; only asking whether they agree does.
func TestValidatorAndBuilderAgreeOnNamespaceTypes(t *testing.T) {
	candidates := []string{
		"personal", "shared", "other", "other_users",
		"Personal", "SHARED", " shared ",
		"", "public", "bogus", "Other Users",
	}

	for _, typ := range candidates {
		cfg := []config.NamespaceConfig{{Type: typ, Prefix: "X/", Separator: "/"}}

		validatorAccepts := config.ValidateNamespaceTypes(cfg) == nil
		builderAccepts := len(buildNamespaces(cfg)) == 1

		if validatorAccepts != builderAccepts {
			t.Errorf("type %q: validator accepts=%v, builder accepts=%v — a type that "+
				"passes startup and is then dropped is exactly the defect this guards",
				typ, validatorAccepts, builderAccepts)
		}
	}
}
