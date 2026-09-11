package provider

import (
	"testing"

	"flexie.io/sag/internal/model"
)

// What a setting IS belongs to the vendor: its name, what it may be, and what it
// is when nobody says. WHERE it is chosen is the agent, because how hard to think
// is a property of the job and one model serves agents doing different ones.

func TestAVendorDeclaresTheEffortAndItsDefault(t *testing.T) {
	for _, vendorKey := range []string{model.VendorOpenAI, model.VendorAnthropic} {
		t.Run(vendorKey, func(t *testing.T) {
			spec, ok := settingNamed(SettingsForVendor(vendorKey), "reasoning_effort")
			if !ok {
				t.Fatalf("%s declares no reasoning effort", vendorKey)
			}
			if spec.Default != "medium" {
				t.Fatalf("default %q, want medium: a default should be right most of "+
					"the time rather than the most expensive one", spec.Default)
			}
			if len(spec.Choices) == 0 {
				t.Fatal("a choice setting arrived with no choices to pick from")
			}
		})
	}
}

// A vendor with no such concept declares nothing, and asking must not invent a
// field for it.
func TestAVendorWithoutTheConceptDeclaresNothing(t *testing.T) {
	if specs := SettingsForVendor("nothing-like-this"); len(specs) != 0 {
		t.Fatalf("%d settings for an unknown vendor, want none", len(specs))
	}
}

// The nearest bag wins, which is the whole point of choosing on the agent: the
// same model serves a classifier that should answer fast and a researcher that
// should not, and the model's own value is only what applies when nobody said.
func TestTheAgentsChoiceBeatsTheModelsAndTheVendors(t *testing.T) {
	spec, _ := settingNamed(SettingsForVendor(model.VendorOpenAI), "reasoning_effort")

	agent := model.Settings{"reasoning_effort": "low"}
	modelBag := model.Settings{"reasoning_effort": "high"}
	vendorBag := model.Settings{"reasoning_effort": "max"}

	if got := spec.Value(agent, modelBag, vendorBag); got != "low" {
		t.Fatalf("with the agent choosing, got %q, want low", got)
	}
	if got := spec.Value(nil, modelBag, vendorBag); got != "high" {
		t.Fatalf("with only the model choosing, got %q, want high", got)
	}
	if got := spec.Value(nil, nil, vendorBag); got != "max" {
		t.Fatalf("with only the vendor choosing, got %q, want max", got)
	}
	if got := spec.Value(nil, nil, nil); got != "medium" {
		t.Fatalf("with nobody choosing, got %q, want the declared default", got)
	}
}

func settingNamed(specs []model.SettingSpec, key string) (model.SettingSpec, bool) {
	for _, spec := range specs {
		if spec.Key == key {
			return spec, true
		}
	}
	return model.SettingSpec{}, false
}
