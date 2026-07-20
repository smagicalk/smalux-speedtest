package serverapp

import (
	"log"
	"log/slog"
)

// sanitizedHTTPErrorWriter is the privacy boundary for net/http's internal
// logger. The standard library may include RemoteAddr, malformed request bytes,
// request paths, or panic text in the supplied payload, so Write intentionally
// discards it and emits only a stable diagnostic event.
type sanitizedHTTPErrorWriter struct {
	logger *slog.Logger
}

func (writer sanitizedHTTPErrorWriter) Write(payload []byte) (int, error) {
	writer.logger.Debug("http server error")
	return len(payload), nil
}

// newSanitizedHTTPErrorLog adapts the structured application logger for
// http.Server.ErrorLog without allowing net/http's unstructured text through.
func newSanitizedHTTPErrorLog(logger *slog.Logger) *log.Logger {
	return log.New(sanitizedHTTPErrorWriter{logger: logger}, "", 0)
}
