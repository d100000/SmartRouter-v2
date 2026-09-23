package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchedulingRoutesRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() { registerSchedulingRoutes(engine.Group("/api")) })
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/scheduling"},
		{http.MethodPost, "/api/scheduling/config"},
		{http.MethodPost, "/api/scheduling/recover"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}
