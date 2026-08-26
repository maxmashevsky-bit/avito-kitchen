package restauranthttp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthAndIntegrationAuthentication(t *testing.T) {
	t.Parallel()
	router := Router(New(nil, "secret", 1024), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, test := range []struct {
		name, method, path string
		want               int
	}{
		{"health", http.MethodGet, "/healthz", http.StatusOK},
		{"authentication required", http.MethodPost, "/integration/v1/orders", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), test.method, test.path, strings.NewReader(`{}`)))
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}
