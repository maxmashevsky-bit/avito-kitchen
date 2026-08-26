package kitchenhttp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthAndGeneratedValidation(t *testing.T) {
	t.Parallel()
	router := Router(New(nil, 1024), slog.New(slog.NewTextHandler(io.Discard, nil)))
	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    string
		want    int
	}{
		{name: "health", method: http.MethodGet, path: "/healthz", want: http.StatusOK},
		{name: "invalid user header", method: http.MethodGet, path: "/api/v1/orders", headers: map[string]string{"X-User-ID": "not-a-uuid"}, want: http.StatusBadRequest},
		{name: "partner auth required", method: http.MethodPut, path: "/partner/v1/menu", body: `{}`, want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequestWithContext(t.Context(), test.method, test.path, strings.NewReader(test.body))
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type = %q", contentType)
			}
		})
	}
}
