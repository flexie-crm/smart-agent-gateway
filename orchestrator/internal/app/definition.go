package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"flexie.io/sag/internal/model"
)

// ValidateDefinition rejects a workflow definition the resolver could not
// honour.
//
// It runs when the version is saved, not when a turn uses it. A definition
// that only fails at run time fails in front of a user, in the middle of a
// conversation, which is the worst place to discover that somebody misspelled
// a field: the user sees an assistant that is broken, and the administrator
// sees nothing at all.
func ValidateDefinition(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("definition is required")
	}

	// Unknown fields are an error rather than a shrug. A workflow saying
	// "reasonning": true would otherwise be accepted, do nothing, and leave an
	// administrator staring at a switch they are sure they turned on.
	var definition model.Definition
	if err := strictUnmarshal(raw, &definition); err != nil {
		return fmt.Errorf("definition: %w", err)
	}
	if definition.Kind != model.DefinitionProfile {
		return fmt.Errorf("definition: kind must be %q", model.DefinitionProfile)
	}
	if definition.Profile == nil {
		return errors.New("definition: a profile is required")
	}
	if definition.Profile.ModelID != nil && *definition.Profile.ModelID <= 0 {
		return errors.New("definition: model_id must name a model")
	}
	if seconds := definition.Profile.ApprovalTTLSeconds; seconds != nil {
		ttl := time.Duration(*seconds) * time.Second
		if !model.ValidApprovalTTL(ttl) {
			return fmt.Errorf("definition: approval_ttl_seconds must be between %s and %s",
				model.MinApprovalTTL, model.MaxApprovalTTL)
		}
	}
	if definition.Profile.Tools != nil {
		for _, name := range *definition.Profile.Tools {
			if name == "" {
				return errors.New("definition: a tool must have a name")
			}
		}
	}
	return nil
}

func strictUnmarshal(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}
