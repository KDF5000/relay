package controlplane_test

import (
	"testing"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func TestRuntimeMatchesAdvertisedModel(t *testing.T) {
	runtime := controlplane.Runtime{ID: "node-a/codex", Provider: "codex", State: "healthy", DefaultModel: "model-default", Models: []string{"model-default", "model-fast"}}
	if !controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{Provider: "codex", Model: "model-fast"}) {
		t.Fatal("advertised model should match")
	}
	if controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{Provider: "codex", Model: "model-large"}) {
		t.Fatal("unadvertised model should not match a runtime with a model catalog")
	}
}

func TestRuntimeMatchesExactInstance(t *testing.T) {
	runtime := controlplane.Runtime{ID: "node-a/codex", Provider: "codex", State: "healthy"}
	if !controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{ID: "node-a/codex", Provider: "codex"}) {
		t.Fatal("requested runtime instance should match")
	}
	if controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{ID: "node-b/codex", Provider: "codex"}) {
		t.Fatal("a different runtime instance must not match")
	}
	if !controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{Provider: "codex"}) {
		t.Fatal("an unbound requirement should match any compatible instance")
	}
}

func TestRuntimeWithoutCatalogAcceptsModelOverride(t *testing.T) {
	runtime := controlplane.Runtime{Provider: "trae", State: "healthy"}
	if !controlplane.RuntimeMatches(runtime, relay.RuntimeRequirement{Provider: "trae", Model: "custom-model"}) {
		t.Fatal("runtime without a model catalog should defer validation to its provider")
	}
}

func TestRuntimeMatchesEffortCapabilityAndModel(t *testing.T) {
	runtime := controlplane.Runtime{ID: "node/codex", Provider: "codex", State: "healthy", SupportsReasoningEffort: true, DefaultModel: "gpt", ModelCatalog: []controlplane.RuntimeModel{{ID: "gpt", SupportedReasoningEfforts: []controlplane.ReasoningEffortOption{{Effort: "xhigh"}}}}}
	request := relay.RuntimeRequirement{Provider: "codex", ReasoningEffort: "xhigh"}
	if !controlplane.RuntimeMatches(runtime, request) {
		t.Fatal("supported effort did not match")
	}
	request.ReasoningEffort = "max"
	if controlplane.RuntimeMatches(runtime, request) {
		t.Fatal("unsupported effort matched")
	}
	request.ReasoningEffort = "xhigh"
	runtime.SupportsReasoningEffort = false
	if controlplane.RuntimeMatches(runtime, request) {
		t.Fatal("old node matched effort request")
	}
	request.ReasoningEffort = ""
	if !controlplane.RuntimeMatches(runtime, request) {
		t.Fatal("default request no longer matches old node")
	}
}
