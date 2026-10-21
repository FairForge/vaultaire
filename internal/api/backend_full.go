package api

import (
	"net/http"

	"go.uber.org/zap"
)

// writeBackendFull answers a write the backend refused because its storage
// is full (engine.ErrBackendFull — Blomp's Swift: 413 "Upload exceeds
// quota."): 507 InsufficientStorage with no Retry-After. Before (prod
// 2026-10-21) it was 503 "please retry", and a client retried for ever
// what could not fit. The operator's to fix, so an Error line; the engine
// counts it in vaultaire_engine_backend_full_total (rule BackendStorageFull).
func writeBackendFull(w http.ResponseWriter, r *http.Request, logger *zap.Logger, op string, err error, fields ...zap.Field) {
	logger.Error("write refused: the backend's storage is full — 507",
		append([]zap.Field{zap.String("op", op), zap.Error(err)}, fields...)...)
	WriteS3ErrorWithContext(w, ErrInsufficientStorage, r.URL.Path, generateRequestID(),
		WithSuggestion("The storage tier this bucket writes to has no room left. Contact support; nothing was stored."))
}
