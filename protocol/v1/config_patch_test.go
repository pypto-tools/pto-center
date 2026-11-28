package protocol

import (
	"encoding/json"
	"testing"
)

func TestConfigPatchUsesReleaseReferences(t *testing.T) {
	var schema Values
	err := json.Unmarshal([]byte(`{"type":"object","additionalProperties":false,"properties":{"limit":{"$ref":"#/$defs/bound"},"label":{"type":"string"}},"required":["label","limit"],"$defs":{"bound":{"type":"integer","maximum":9}}}`), &schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		patch Values
		valid bool
	}{
		{Values{"limit": 8}, true}, {Values{"label": ""}, true}, {Values{"limit": 10}, false},
		{Values{"limit": false}, false}, {Values{"oops": nil}, false}, {Values{"label": nil}, false},
	} {
		if err := ValidateConfigPatch(schema, test.patch); (err == nil) != test.valid {
			t.Fatal(test.patch, err)
		}
	}
}
