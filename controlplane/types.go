package controlplane

import (
	"strings"
	"time"

	"github.com/KDF5000/relay"
	semver "github.com/Masterminds/semver/v3"
)

type Runtime struct {
	ID           string         `json:"id"`
	Provider     string         `json:"provider"`
	Version      string         `json:"version,omitempty"`
	State        string         `json:"state,omitempty"`
	Message      string         `json:"message,omitempty"`
	DefaultModel string         `json:"default_model,omitempty"`
	Models       []string       `json:"models,omitempty"`
	ModelCatalog []RuntimeModel `json:"model_catalog,omitempty"`
	CheckedAt    *time.Time     `json:"checked_at,omitempty"`
}

type RuntimeModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

func RuntimeMatches(runtime Runtime, requirement relay.RuntimeRequirement) bool {
	if requirement.ID != "" && runtime.ID != requirement.ID {
		return false
	}
	if runtime.Provider != requirement.Provider || runtime.State == "unhealthy" {
		return false
	}
	if requirement.Model != "" && len(runtime.Models) > 0 {
		modelAvailable := runtime.DefaultModel == requirement.Model
		for _, model := range runtime.Models {
			modelAvailable = modelAvailable || model == requirement.Model
		}
		if !modelAvailable {
			return false
		}
	}
	if requirement.Version == "" {
		return true
	}
	actual, err := semver.NewVersion(runtime.Version)
	if err != nil {
		return runtime.Version == requirement.Version
	}
	constraint, err := semver.NewConstraint(requirement.Version)
	return err == nil && constraint.Check(actual)
}

// RuntimeInstanceID returns the stable default identity for a runtime on a node.
// Providers may supply an explicit ID; this fallback keeps older node configs
// compatible while making every advertised runtime addressable.
func RuntimeInstanceID(nodeID, provider string) string {
	return strings.TrimSpace(nodeID) + "/" + strings.TrimSpace(provider)
}

type Capability struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type NodeRegistration struct {
	ID              string            `json:"id"`
	Version         string            `json:"version,omitempty"`
	ProtocolVersion string            `json:"protocol_version"`
	Labels          map[string]string `json:"labels,omitempty"`
	Runtimes        []Runtime         `json:"runtimes"`
	Capabilities    []Capability      `json:"capabilities,omitempty"`
	Capacity        int               `json:"capacity"`
}

type Node struct {
	NodeRegistration
	Active   int       `json:"active"`
	LastSeen time.Time `json:"last_seen"`
	State    string    `json:"state"`
}

const (
	NodeOnline  = "online"
	NodeOffline = "offline"
)

type Assignment struct {
	RunID            string        `json:"run_id"`
	AttemptID        string        `json:"attempt_id"`
	LeaseToken       string        `json:"lease_token"`
	LeaseExpiresAt   time.Time     `json:"lease_expires_at"`
	Request          relay.Request `json:"request"`
	RuntimeSessionID string        `json:"runtime_session_id,omitempty"`
}

type LeaseUpdate struct {
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
	CancelRequested bool      `json:"cancel_requested"`
	CancelReason    string    `json:"cancel_reason,omitempty"`
}

type CancelRequest struct {
	Reason      string `json:"reason,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
}

type Completion struct {
	LeaseToken string       `json:"lease_token"`
	Result     relay.Result `json:"result"`
}

type Failure struct {
	LeaseToken string `json:"lease_token"`
	Error      string `json:"error"`
}

type NodeEvent struct {
	LeaseToken string `json:"lease_token"`
	Type       string `json:"type"`
	Data       any    `json:"data,omitempty"`
}
