package httpapi

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

type Handler struct {
	service     *controlplane.Service
	mux         *http.ServeMux
	handler     http.Handler
	auth        Authenticator
	inspections inspectionBroker
}

//go:embed console/*
var consoleFiles embed.FS

func NewHandler(service *controlplane.Service) *Handler {
	return NewHandlerWithAuth(service, nil)
}

func NewHandlerWithAuth(service *controlplane.Service, auth Authenticator) *Handler {
	h := &Handler{service: service, mux: http.NewServeMux(), auth: auth}
	h.inspections.calls = make(map[string]*inspectionCall)
	h.mux.HandleFunc("GET /v1/runs/{runID}/workspace", h.inspect)
	h.mux.HandleFunc("POST /v1/nodes/{nodeID}/inspections/claim", h.claimInspection)
	h.mux.HandleFunc("POST /v1/nodes/{nodeID}/inspections/{inspectionID}", h.finishInspection)
	assets, err := fs.Sub(consoleFiles, "console")
	if err != nil {
		panic(err)
	}
	h.mux.Handle("GET /console/", http.StripPrefix("/console/", http.FileServer(http.FS(assets))))
	h.mux.HandleFunc("GET /console", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusTemporaryRedirect)
	})
	h.mux.HandleFunc("POST /v1/console/session", h.consoleSession)
	h.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	h.mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, relay.CurrentBuild())
	})
	h.mux.HandleFunc("POST /v1/runs", h.submit)
	h.mux.HandleFunc("GET /v1/runs", h.listRuns)
	h.mux.HandleFunc("GET /v1/sessions/{sessionID}/runs", h.sessionRuns)
	h.mux.HandleFunc("GET /v1/runs/{runID}", h.getRun)
	h.mux.HandleFunc("GET /v1/runs/{runID}/attempts", h.attempts)
	h.mux.HandleFunc("GET /v1/runs/{runID}/interactions", h.interactions)
	h.mux.HandleFunc("POST /v1/interactions/{interactionID}/resolve", h.resolveInteraction)
	h.mux.HandleFunc("GET /v1/runs/{runID}/events", h.events)
	h.mux.HandleFunc("GET /v1/runs/{runID}/events/stream", h.streamEvents)
	h.mux.HandleFunc("GET /v1/runs/{runID}/artifacts", h.artifacts)
	h.mux.HandleFunc("GET /v1/artifacts/{artifactID}", h.downloadArtifact)
	h.mux.HandleFunc("POST /v1/runs/{runID}/cancel", h.cancelRun)
	h.mux.HandleFunc("GET /v1/nodes", h.nodes)
	h.mux.HandleFunc("PUT /v1/nodes/{nodeID}/capacity", h.updateNodeCapacity)
	h.mux.HandleFunc("POST /v1/nodes/register", h.register)
	h.mux.HandleFunc("POST /v1/nodes/{nodeID}/heartbeat", h.heartbeat)
	h.mux.HandleFunc("POST /v1/nodes/{nodeID}/claim", h.claim)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/start", h.start)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/renew", h.renew)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/events", h.appendEvent)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/artifacts", h.uploadArtifact)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/capability-calls/reserve", h.reserveCapability)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/capability-calls/{callID}/finish", h.finishCapability)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/interactions", h.createInteraction)
	h.mux.HandleFunc("GET /v1/attempts/{attemptID}/interactions/{interactionID}", h.getInteraction)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/complete", h.complete)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/fail", h.fail)
	h.mux.HandleFunc("POST /v1/attempts/{attemptID}/cancelled", h.acknowledgeCancellation)
	h.handler = authenticate(h.mux, auth)
	return h
}

func (h *Handler) consoleSession(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &value) {
		return
	}
	if h.auth != nil {
		scope, ok := h.auth.Authenticate(value.Token)
		if !ok || scope.Kind != controlplane.AccessHost {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid host token"})
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_host_token", Value: value.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	value, err := h.service.ListRuns(r.Context(), limit)
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) sessionRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	value, err := h.service.SessionRuns(r.Context(), r.PathValue("sessionID"), limit)
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) attempts(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Attempts(r.Context(), r.PathValue("runID"))
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) interactions(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Interactions(r.Context(), r.PathValue("runID"))
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) resolveInteraction(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Response json.RawMessage `json:"response"`
	}
	if !decode(w, r, &value) {
		return
	}
	result, err := h.service.ResolveInteraction(r.Context(), r.PathValue("interactionID"), value.Response)
	respond(w, http.StatusOK, result, err)
}
func (h *Handler) createInteraction(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Assignment controlplane.Assignment  `json:"assignment"`
		Request    relay.InteractionRequest `json:"request"`
	}
	if !decode(w, r, &value) {
		return
	}
	if value.Assignment.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	result, err := h.service.CreateInteraction(r.Context(), value.Assignment, value.Request)
	respond(w, http.StatusCreated, result, err)
}
func (h *Handler) getInteraction(w http.ResponseWriter, r *http.Request) {
	a := controlplane.Assignment{RunID: r.URL.Query().Get("run_id"), AttemptID: r.PathValue("attemptID"), LeaseToken: r.Header.Get("X-Relay-Lease-Token")}
	result, err := h.service.GetInteraction(r.Context(), a, r.PathValue("interactionID"))
	respond(w, http.StatusOK, result, err)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.handler.ServeHTTP(w, r) }
func (h *Handler) artifacts(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Artifacts(r.Context(), r.PathValue("runID"))
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	metadata, reader, err := h.service.OpenArtifact(r.Context(), r.PathValue("artifactID"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer reader.Close()
	if metadata.ContentType != "" {
		w.Header().Set("Content-Type", metadata.ContentType)
	}
	if metadata.Name != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, metadata.Name))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	_, _ = io.Copy(w, reader)
}
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	var value relay.Request
	if !decode(w, r, &value) {
		return
	}
	run, err := h.service.Submit(r.Context(), value)
	respond(w, http.StatusAccepted, run, err)
}
func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.GetRun(r.Context(), r.PathValue("runID"))
	respond(w, http.StatusOK, value, err)
}
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Events(r.Context(), r.PathValue("runID"))
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	afterValue := r.URL.Query().Get("after")
	if afterValue == "" {
		afterValue = r.Header.Get("Last-Event-ID")
	}
	after := 0
	if afterValue != "" {
		value, err := strconv.Atoi(afterValue)
		if err != nil || value < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "after must be a non-negative event sequence"})
			return
		}
		after = value
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming is unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	writeEvents := func(values []relay.Event) error {
		for _, event := range values {
			if event.Sequence <= after {
				continue
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: relay.event\ndata: %s\n\n", event.Sequence, encoded); err != nil {
				return err
			}
			after = event.Sequence
		}
		flusher.Flush()
		return nil
	}
	poll := time.NewTicker(time.Second)
	keepalive := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
	for {
		updated := h.service.RunUpdates(r.PathValue("runID"))
		values, err := h.service.EventsAfter(r.Context(), r.PathValue("runID"), after)
		if err != nil || writeEvents(values) != nil {
			return
		}
		run, err := h.service.GetRun(r.Context(), r.PathValue("runID"))
		if err != nil {
			return
		}
		if streamTerminal(run.Status) {
			values, err := h.service.EventsAfter(r.Context(), r.PathValue("runID"), after)
			if err == nil {
				_ = writeEvents(values)
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-updated:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
		}
	}
}

func streamTerminal(status relay.RunStatus) bool {
	return status == relay.RunSucceeded || status == relay.RunFailed || status == relay.RunCancelled
}
func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	var value controlplane.CancelRequest
	if !decode(w, r, &value) {
		return
	}
	run, err := h.service.CancelRun(r.Context(), r.PathValue("runID"), value)
	respond(w, http.StatusOK, run, err)
}
func (h *Handler) nodes(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Nodes(r.Context())
	respondList(w, http.StatusOK, value, err)
}
func (h *Handler) updateNodeCapacity(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Capacity int `json:"capacity"`
	}
	if !decode(w, r, &value) {
		return
	}
	node, err := h.service.UpdateNodeCapacity(r.Context(), r.PathValue("nodeID"), value.Capacity)
	respond(w, http.StatusOK, node, err)
}
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var value controlplane.NodeRegistration
	if !decode(w, r, &value) {
		return
	}
	if scope, ok := controlplane.AccessFrom(r.Context()); ok && scope.Kind == controlplane.AccessNode && scope.Subject != "" && scope.Subject != value.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "node identity mismatch"})
		return
	}
	node, err := h.service.RegisterNode(r.Context(), value)
	respond(w, http.StatusOK, node, err)
}
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	node, err := h.service.Heartbeat(r.Context(), r.PathValue("nodeID"))
	respond(w, http.StatusOK, node, err)
}
func (h *Handler) claim(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Claim(r.Context(), r.PathValue("nodeID"))
	if errors.Is(err, controlplane.ErrNoAssignment) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respond(w, http.StatusOK, value, err)
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	var value controlplane.Assignment
	if !decode(w, r, &value) {
		return
	}
	if value.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	respondEmpty(w, h.service.Start(r.Context(), value))
}
func (h *Handler) renew(w http.ResponseWriter, r *http.Request) {
	var value controlplane.Assignment
	if !decode(w, r, &value) {
		return
	}
	if value.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	update, err := h.service.Renew(r.Context(), value)
	respond(w, http.StatusOK, update, err)
}
func (h *Handler) appendEvent(w http.ResponseWriter, r *http.Request) {
	var value struct {
		EventID    string          `json:"event_id"`
		RunID      string          `json:"run_id"`
		LeaseToken string          `json:"lease_token"`
		Type       string          `json:"type"`
		Data       json.RawMessage `json:"data,omitempty"`
	}
	if !decode(w, r, &value) {
		return
	}
	respondEmpty(w, h.service.AppendEvent(r.Context(), value.RunID, r.PathValue("attemptID"), value.LeaseToken, value.Type, value.Data, value.EventID))
}
func (h *Handler) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	assignment := controlplane.Assignment{RunID: r.URL.Query().Get("run_id"), AttemptID: r.PathValue("attemptID"), LeaseToken: r.Header.Get("X-Relay-Lease-Token")}
	artifact := relay.Artifact{Type: r.URL.Query().Get("type"), Name: r.URL.Query().Get("name"), ContentType: r.Header.Get("Content-Type")}
	if assignment.RunID == "" || assignment.LeaseToken == "" || artifact.Type == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id, lease token, and artifact type are required"})
		return
	}
	reader := http.MaxBytesReader(w, r.Body, 256<<20)
	value, err := h.service.UploadArtifact(r.Context(), assignment, artifact, reader)
	respond(w, http.StatusCreated, value, err)
}
func (h *Handler) reserveCapability(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Assignment controlplane.Assignment `json:"assignment"`
		Key        string                  `json:"idempotency_key"`
		Hash       string                  `json:"request_hash"`
		Request    relay.CapabilityRequest `json:"request"`
	}
	if !decode(w, r, &value) {
		return
	}
	if value.Assignment.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	result, err := h.service.ReserveCapability(r.Context(), value.Assignment, value.Key, value.Hash, value.Request)
	respond(w, http.StatusOK, result, err)
}
func (h *Handler) finishCapability(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Assignment  controlplane.Assignment     `json:"assignment"`
		Reservation relay.CapabilityReservation `json:"reservation"`
		Result      relay.CapabilityResult      `json:"result"`
		Error       string                      `json:"error,omitempty"`
	}
	if !decode(w, r, &value) {
		return
	}
	if value.Assignment.AttemptID != r.PathValue("attemptID") || value.Reservation.CallID != r.PathValue("callID") {
		writeError(w, errors.New("capability call identity mismatch"))
		return
	}
	respondEmpty(w, h.service.FinishCapability(r.Context(), value.Assignment, value.Reservation, value.Result, value.Error))
}
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Assignment controlplane.Assignment `json:"assignment"`
		Result     relay.Result            `json:"result"`
	}
	if !decode(w, r, &value) {
		return
	}
	if value.Assignment.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	respondEmpty(w, h.service.Complete(r.Context(), value.Assignment, value.Result))
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request) {
	var value struct {
		Assignment controlplane.Assignment `json:"assignment"`
		Error      string                  `json:"error"`
	}
	if !decode(w, r, &value) {
		return
	}
	if value.Assignment.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	respondEmpty(w, h.service.Fail(r.Context(), value.Assignment, value.Error))
}
func (h *Handler) acknowledgeCancellation(w http.ResponseWriter, r *http.Request) {
	var value controlplane.Assignment
	if !decode(w, r, &value) {
		return
	}
	if value.AttemptID != r.PathValue("attemptID") {
		writeError(w, errors.New("attempt ID mismatch"))
		return
	}
	respondEmpty(w, h.service.AcknowledgeCancellation(r.Context(), value))
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, value)
}
func respondList[T any](w http.ResponseWriter, status int, value []T, err error) {
	if value == nil {
		value = []T{}
	}
	respond(w, status, value, err)
}
func respondEmpty(w http.ResponseWriter, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, controlplane.ErrNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, controlplane.ErrInvalidLease) {
		status = http.StatusUnauthorized
	}
	if errors.Is(err, controlplane.ErrInvalidTransition) {
		status = http.StatusConflict
	}
	if errors.Is(err, controlplane.ErrRunCancelled) {
		status = http.StatusGone
	}
	if errors.Is(err, controlplane.ErrIncompatibleProtocol) {
		status = http.StatusUpgradeRequired
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
