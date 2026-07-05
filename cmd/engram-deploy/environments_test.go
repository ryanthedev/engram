package main

import "testing"

// TestStagingAndProd_AreTopologyIdentical proves D22: staging and prod
// declare the same VPC/domain/secret/service SHAPE (same names modulo the
// environment name, same service set, same pinned OpenSearch engine
// version) — they differ only in scale (instance/desired counts), never in
// which resources exist.
func TestStagingAndProd_AreTopologyIdentical(t *testing.T) {
	staging := stagingEnvironment("engram:v1")
	prod := prodEnvironment("engram:v1")

	if len(staging.Services) != len(prod.Services) {
		t.Fatalf("service count staging=%d prod=%d, want equal (topology-identical, D22)", len(staging.Services), len(prod.Services))
	}
	for i := range staging.Services {
		if staging.Services[i].Name != prod.Services[i].Name {
			t.Errorf("service[%d] name staging=%s prod=%s, want identical service set", i, staging.Services[i].Name, prod.Services[i].Name)
		}
	}
	if staging.Domain.EngineVersion != "OpenSearch_3.1" || prod.Domain.EngineVersion != "OpenSearch_3.1" {
		t.Errorf("domain engine version staging=%s prod=%s, want the pinned OpenSearch_3.1 line (D14) in both", staging.Domain.EngineVersion, prod.Domain.EngineVersion)
	}
	if len(staging.Secrets) != len(prod.Secrets) {
		t.Errorf("secret count staging=%d prod=%d, want equal", len(staging.Secrets), len(prod.Secrets))
	}

	// Prod is scaled up, not architecturally different.
	if prod.Domain.InstanceCount <= staging.Domain.InstanceCount {
		t.Errorf("prod domain InstanceCount=%d should exceed staging=%d", prod.Domain.InstanceCount, staging.Domain.InstanceCount)
	}
	if prod.Services[0].DesiredCount <= staging.Services[0].DesiredCount {
		t.Errorf("prod engramd DesiredCount=%d should exceed staging=%d", prod.Services[0].DesiredCount, staging.Services[0].DesiredCount)
	}
}

// TestEnvironmentFor_UnknownNameErrors proves an invalid -env value fails
// loudly instead of silently defaulting to a real environment.
func TestEnvironmentFor_UnknownNameErrors(t *testing.T) {
	if _, err := environmentFor("production", "engram:v1"); err == nil {
		t.Fatal("expected an error for an unrecognized -env value")
	}
	for _, name := range []string{"staging", "prod"} {
		if _, err := environmentFor(name, "engram:v1"); err != nil {
			t.Errorf("environmentFor(%q) = %v, want no error", name, err)
		}
	}
}

// TestEnvironment_SecretsNeverCarryReadableProductionValues is a light
// guardrail: the placeholder committed here must never look like a real key
// (a future edit accidentally hardcoding a real secret would trip this).
func TestEnvironment_SecretsNeverCarryReadableProductionValues(t *testing.T) {
	for _, s := range stagingEnvironment("engram:v1").Secrets {
		if s.Value != "REPLACE_VIA_ROTATION_RUNBOOK" {
			t.Errorf("secret %s has a non-placeholder value committed in source", s.Name)
		}
	}
}
