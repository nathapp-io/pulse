package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBatchTargetDeclarationRoundTrip(t *testing.T) {
	for _, body := range []string{`{}`, `{"targets":null}`, `{"targets":[]}`} {
		var incoming Batch
		if err := json.Unmarshal([]byte(body), &incoming); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(incoming)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Batch
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if wantNil := body != `{"targets":[]}`; (decoded.Targets == nil) != wantNil {
			t.Errorf("%s -> %s: targets %#v, want nil=%v", body, encoded, decoded.Targets, wantNil)
		}
	}
}

func TestValidateInventoryRevision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int64
		list     []TargetSpec
		valid    bool
	}{
		{"negative", -1, []TargetSpec{}, false}, {"missing list", 1, nil, false},
		{"versioned clear", 1, []TargetSpec{}, true}, {"legacy", 0, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Batch{Prober: ProberInfo{Name: "p"}, InventoryRevision: tc.revision, Targets: tc.list, Results: []Result{{Target: "api", At: time.Now()}}}
			if err := b.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}
