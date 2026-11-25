package model

import (
	"encoding/json"
	"testing"
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
