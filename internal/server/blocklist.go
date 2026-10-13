package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/tggo/cerber/internal/modelblock"
	"github.com/tggo/cerber/internal/usage"
)

// errDialect selects the error-body shape of a blocked-model rejection, matching
// the endpoint the client called.
type errDialect int

const (
	dialectOpenAI errDialect = iota
	dialectAnthropic
)

// SetModelBlocklist installs the global model blocklist (config-seeded plus
// operator masks). nil keeps the current (empty, in-memory) list. Call before
// Handler().
func (s *Server) SetModelBlocklist(l *modelblock.List) {
	if l != nil {
		s.blocklist = l
	}
}

// rejectBlocked answers 403 and returns true when the requested model — or the
// alias-resolved canonical one — is on the global blocklist. No upstream call is
// made; the attempt is recorded as an error (visible in the dashboard) with no
// tokens, hence no cost.
func (s *Server) rejectBlocked(w http.ResponseWriter, r *http.Request, d errDialect, requested, canonical string) bool {
	mask, ok := s.blocklist.Blocked(requested, canonical)
	if !ok {
		return false
	}
	shown := requested
	if strings.TrimSpace(shown) == "" {
		shown = canonical
	}
	s.log.Info("blocked model rejected", zap.String("model", shown), zap.String("mask", mask))
	s.record(r.Context(), usage.Event{Model: canonical, IsError: true})
	writeBlockedError(w, d, shown)
	return true
}

func writeBlockedError(w http.ResponseWriter, d errDialect, model string) {
	msg := fmt.Sprintf("model %q is blocked on this cerber instance", model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if d == dialectAnthropic {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "permission_error", "message": msg},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": "model_blocked"},
	})
}

// dropBlocked removes blocked models from fallback targets (index >= 1). The
// primary (index 0) is checked by the handler and answered with 403 instead.
func (s *Server) dropBlocked(targets []string) []string {
	if len(targets) < 2 {
		return targets
	}
	out := targets[:1:1]
	for _, t := range targets[1:] {
		if _, blocked := s.blocklist.Blocked(t, s.catalog.Canonical(t)); blocked {
			s.log.Info("fallback target skipped: model is blocked", zap.String("model", t))
			continue
		}
		out = append(out, t)
	}
	return out
}

// visibleModels returns a provider's discovered models minus blocked ones.
func (s *Server) visibleModels(name string) []string {
	all := s.providerModels(name)
	out := make([]string, 0, len(all))
	for _, m := range all {
		if _, blocked := s.blocklist.Blocked(m); !blocked {
			out = append(out, m)
		}
	}
	return out
}

// multipartModel reads the "model" form field from a multipart/form-data body
// ("" when absent or unparseable).
func multipartModel(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if p.FormName() == "model" {
			b, _ := io.ReadAll(io.LimitReader(p, 256))
			return strings.TrimSpace(string(b))
		}
	}
}

type blockedView struct {
	Mask    string `json:"mask"`
	Source  string `json:"source"`
	Matches int    `json:"matches"` // currently advertised models this mask covers
}

// blocklistView lists every mask with the number of currently advertised
// (discovered) models it matches.
func (s *Server) blocklistView() []blockedView {
	s.provMu.Lock()
	seen := map[string]bool{}
	var ids []string
	for _, models := range s.provModels {
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				ids = append(ids, m)
			}
		}
	}
	s.provMu.Unlock()
	entries := s.blocklist.Entries()
	out := make([]blockedView, 0, len(entries))
	for _, e := range entries {
		n := 0
		for _, id := range ids {
			if modelblock.Match(e.Mask, id) {
				n++
			}
		}
		out = append(out, blockedView{Mask: e.Mask, Source: e.Source, Matches: n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func (s *Server) writeBlocklist(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"blocked": s.blocklistView()})
}

// handleBlockedList returns GET /admin/blocked-models.
func (s *Server) handleBlockedList(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	s.writeBlocklist(w, http.StatusOK)
}

// handleBlockedAdd serves POST /admin/blocked-models with {"mask": "..."}: 201
// when added, 200 when already present, 400 for an invalid mask.
func (s *Server) handleBlockedAdd(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	var req struct {
		Mask string `json:"mask"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	norm, added, err := s.blocklist.Add(req.Mask)
	if err != nil {
		if _, verr := modelblock.Normalize(req.Mask); verr != nil {
			writeError(w, http.StatusBadRequest, verr.Error())
			return
		}
		s.log.Error("persist blocked model", zap.String("mask", norm), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not persist blocklist: "+err.Error())
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
		s.log.Info("model mask blocked", zap.String("mask", norm))
	}
	s.writeBlocklist(w, status)
}

// handleBlockedRemove serves DELETE /admin/blocked-models?mask=... and
// POST /admin/blocked-models/delete with {"mask": "..."}.
func (s *Server) handleBlockedRemove(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	mask := r.URL.Query().Get("mask")
	if mask == "" {
		var req struct {
			Mask string `json:"mask"`
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
		}
		mask = req.Mask
	}
	if _, err := modelblock.Normalize(mask); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	found, err := s.blocklist.Remove(mask)
	switch {
	case errors.Is(err, modelblock.ErrConfigEntry):
		writeError(w, http.StatusConflict, "mask comes from config (blocked_models) and cannot be removed at runtime")
	case err != nil:
		s.log.Error("persist blocklist removal", zap.String("mask", mask), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not persist blocklist: "+err.Error())
	case !found:
		writeError(w, http.StatusNotFound, "no such mask: "+mask)
	default:
		s.log.Info("model mask unblocked", zap.String("mask", mask))
		s.writeBlocklist(w, http.StatusOK)
	}
}
