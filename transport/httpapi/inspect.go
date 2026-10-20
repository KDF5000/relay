package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/KDF5000/relay"
)

// Inspection is a read-only RPC for either a run-prepared workspace or a
// WorkspaceSpec resolved directly on the selected Node.
type Inspection struct {
	ID        string              `json:"id"`
	RunID     string              `json:"runId,omitempty"`
	Workspace relay.WorkspaceSpec `json:"workspace,omitempty"`
	Operation string              `json:"operation"`
	Path      string              `json:"path"`
}
type InspectionResult struct {
	Encoding    string      `json:"encoding,omitempty"`
	ContentType string      `json:"content_type,omitempty"`
	Root        string      `json:"root"`
	Path        string      `json:"path"`
	Content     string      `json:"content"`
	Entries     []FileEntry `json:"entries"`
	Git         *GitStatus  `json:"git,omitempty"`
	Error       string      `json:"error,omitempty"`
}
type GitStatus struct {
	Repository bool   `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Detached   bool   `json:"detached,omitempty"`
}
type FileEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
}
type inspectionCall struct {
	node    string
	request Inspection
	claimed bool
	result  chan InspectionResult
}
type inspectionBroker struct {
	mu    sync.Mutex
	calls map[string]*inspectionCall
}

func (h *Handler) inspect(w http.ResponseWriter, r *http.Request) {
	run, err := h.service.GetRun(r.Context(), r.PathValue("runID"))
	if err != nil {
		respond(w, 200, nil, err)
		return
	}
	attempts, err := h.service.Attempts(r.Context(), run.ID)
	if err != nil || len(attempts) == 0 {
		writeJSON(w, 409, map[string]string{"error": "workspace has not been prepared"})
		return
	}
	request := Inspection{ID: rand.Text(), RunID: run.ID, Operation: r.URL.Query().Get("operation"), Path: r.URL.Query().Get("path")}
	h.dispatchInspection(w, r, attempts[len(attempts)-1].NodeID, request)
}

// inspectNodeWorkspace performs a read-only inspection directly on the
// requested Node. Unlike run inspection, it does not require that the Node has
// prepared this workspace for an earlier run.
func (h *Handler) inspectNodeWorkspace(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.service.Nodes(r.Context())
	if err != nil {
		respond(w, http.StatusOK, nil, err)
		return
	}
	nodeID := r.PathValue("nodeID")
	found := false
	for _, node := range nodes {
		if node.ID != nodeID {
			continue
		}
		found = true
		if node.State != "online" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace node is offline"})
			return
		}
		break
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace node not found"})
		return
	}
	workspaceJSON := r.URL.Query().Get("workspace")
	var workspace relay.WorkspaceSpec
	if workspaceJSON == "" || json.Unmarshal([]byte(workspaceJSON), &workspace) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a valid workspace is required"})
		return
	}
	if workspace.Kind == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace kind is required"})
		return
	}
	request := Inspection{ID: rand.Text(), Workspace: workspace, Operation: r.URL.Query().Get("operation"), Path: r.URL.Query().Get("path")}
	h.dispatchInspection(w, r, nodeID, request)
}

func (h *Handler) dispatchInspection(w http.ResponseWriter, r *http.Request, nodeID string, request Inspection) {
	if request.Operation != "list" && request.Operation != "read" && request.Operation != "diff" && request.Operation != "git-status" {
		writeJSON(w, 400, map[string]string{"error": "invalid inspection operation"})
		return
	}
	call := &inspectionCall{node: nodeID, request: request, result: make(chan InspectionResult, 1)}
	h.inspections.mu.Lock()
	h.inspections.calls[request.ID] = call
	h.inspections.mu.Unlock()
	defer func() { h.inspections.mu.Lock(); delete(h.inspections.calls, request.ID); h.inspections.mu.Unlock() }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case result := <-call.result:
		if result.Error != "" {
			writeJSON(w, 422, result)
		} else {
			writeJSON(w, 200, result)
		}
	case <-timer.C:
		writeJSON(w, 504, map[string]string{"error": "workspace node unavailable or inspection timed out"})
	case <-r.Context().Done():
	}
}
func (h *Handler) claimInspection(w http.ResponseWriter, r *http.Request) {
	h.inspections.mu.Lock()
	defer h.inspections.mu.Unlock()
	for _, call := range h.inspections.calls {
		if call.node == r.PathValue("nodeID") && !call.claimed {
			call.claimed = true
			writeJSON(w, 200, call.request)
			return
		}
	}
	writeJSON(w, 200, Inspection{})
}
func (h *Handler) finishInspection(w http.ResponseWriter, r *http.Request) {
	var result InspectionResult
	if !decode(w, r, &result) {
		return
	}
	h.inspections.mu.Lock()
	defer h.inspections.mu.Unlock()
	call := h.inspections.calls[r.PathValue("inspectionID")]
	if call == nil || call.node != r.PathValue("nodeID") {
		writeJSON(w, 404, map[string]string{"error": "inspection expired"})
		return
	}
	select {
	case call.result <- result:
	default:
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (c *Client) Inspect(ctx context.Context, runID, operation, path string) (InspectionResult, error) {
	var result InspectionResult
	err := c.do(ctx, "GET", "/v1/runs/"+runID+"/workspace?operation="+url.QueryEscape(operation)+"&path="+url.QueryEscape(path), nil, &result)
	return result, err
}

func (c *Client) InspectNodeWorkspace(ctx context.Context, nodeID string, workspace relay.WorkspaceSpec, operation, path string) (InspectionResult, error) {
	encodedWorkspace, err := json.Marshal(workspace)
	if err != nil {
		return InspectionResult{}, err
	}
	var result InspectionResult
	err = c.do(ctx, "GET", "/v1/nodes/"+url.PathEscape(nodeID)+"/workspace?operation="+url.QueryEscape(operation)+"&path="+url.QueryEscape(path)+"&workspace="+url.QueryEscape(string(encodedWorkspace)), nil, &result)
	return result, err
}
func (c *Client) ClaimInspection(ctx context.Context, nodeID string) (Inspection, error) {
	var request Inspection
	err := c.do(ctx, "POST", "/v1/nodes/"+nodeID+"/inspections/claim", nil, &request)
	return request, err
}
func (c *Client) FinishInspection(ctx context.Context, nodeID, id string, result InspectionResult) error {
	return c.do(ctx, "POST", "/v1/nodes/"+nodeID+"/inspections/"+id, result, nil)
}
