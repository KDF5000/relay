package httpapi

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Inspection is a read-only RPC to the node that prepared a run's workspace.
type Inspection struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	Operation string `json:"operation"`
	Path      string `json:"path"`
}
type InspectionResult struct {
	Root    string      `json:"root"`
	Path    string      `json:"path"`
	Content string      `json:"content"`
	Entries []FileEntry `json:"entries"`
	Error   string      `json:"error,omitempty"`
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
	if request.Operation != "list" && request.Operation != "read" && request.Operation != "diff" {
		writeJSON(w, 400, map[string]string{"error": "invalid inspection operation"})
		return
	}
	call := &inspectionCall{node: attempts[len(attempts)-1].NodeID, request: request, result: make(chan InspectionResult, 1)}
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
func (c *Client) ClaimInspection(ctx context.Context, nodeID string) (Inspection, error) {
	var request Inspection
	err := c.do(ctx, "POST", "/v1/nodes/"+nodeID+"/inspections/claim", nil, &request)
	return request, err
}
func (c *Client) FinishInspection(ctx context.Context, nodeID, id string, result InspectionResult) error {
	return c.do(ctx, "POST", "/v1/nodes/"+nodeID+"/inspections/"+id, result, nil)
}
