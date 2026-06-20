package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdditionalEndpointsBasePath_JSONRoundTrip(t *testing.T) {
	spec := KrakenDAutoConfigSpec{AdditionalEndpointsBasePath: "/api/v1/quote"}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"additionalEndpointsBasePath":"/api/v1/quote"`) {
		t.Fatalf("missing field: %s", data)
	}
	var rt KrakenDAutoConfigSpec
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt.AdditionalEndpointsBasePath != "/api/v1/quote" {
		t.Fatalf("round-trip mismatch: %q", rt.AdditionalEndpointsBasePath)
	}
}

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
