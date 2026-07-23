package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPublicHealthEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := (&Server{}).newRouter()

	tests := []struct {
		path string
		body string
	}{
		{path: "/healthz", body: `{"status":"ok"}`},
		{path: "/readyz", body: `{"status":"ready"}`},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if response.Body.String() != tt.body {
				t.Fatalf("body = %q, want %q", response.Body.String(), tt.body)
			}
		})
	}
}
