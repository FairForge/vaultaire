package api

import (
	"encoding/json"
	"net/http"
)

func getRequestID(w http.ResponseWriter) string {
	return w.Header().Get("X-Request-Id")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type listResponse struct {
	Object     string        `json:"object"`
	Data       []interface{} `json:"data"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitempty"`
	TotalCount int           `json:"total_count"`
	RequestID  string        `json:"request_id"`
}

func writeListResponse(w http.ResponseWriter, items []interface{}, hasMore bool, nextCursor string, totalCount int) {
	resp := listResponse{
		Object:     "list",
		Data:       items,
		HasMore:    hasMore,
		NextCursor: nextCursor,
		TotalCount: totalCount,
		RequestID:  getRequestID(w),
	}
	if resp.Data == nil {
		resp.Data = []interface{}{}
	}
	writeJSON(w, http.StatusOK, resp)
}
