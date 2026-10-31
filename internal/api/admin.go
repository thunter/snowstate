package api

import (
	"net/http"

	"github.com/thunter/snowstate/pkg/snowstate/service"
)

func (h *Handler) admin(w http.ResponseWriter, r *http.Request, _ service.Principal, _ *reqInfo) {
	h.writeError(w, r, http.StatusNotFound, "not found")
}
