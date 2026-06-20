package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdditionalEndpoints_JSONRoundTrip(t *testing.T) {
	spec := KrakenDAutoConfigSpec{
		AdditionalEndpoints: []AdditionalEndpoint{
			{Endpoint: "/liveness", Encoding: "no-op"},
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"additionalEndpoints"`) {
		t.Fatalf("expected additionalEndpoints key, got %s", data)
	}

	var rt KrakenDAutoConfigSpec
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rt.AdditionalEndpoints) != 1 || rt.AdditionalEndpoints[0].Endpoint != "/liveness" {
		t.Fatalf("round-trip mismatch: %+v", rt.AdditionalEndpoints)
	}
}
