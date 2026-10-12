package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/debuglog"
)

func TestRequestLoggerAddsRequestIDHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	logger := slog.New(slog.DiscardHandler)
	handler := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), logger, debuglog.New(&config.Config{}))

	handler.ServeHTTP(recorder, request)
	requestID := recorder.Header().Get("request-id")
	if len(requestID) != 8 || recorder.Header().Get("x-request-id") != requestID {
		t.Fatalf("request headers = %#v", recorder.Header())
	}
}
