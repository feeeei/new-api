package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestSetUpLoggerMasksGeminiQueryAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousWriter := gin.DefaultWriter
	var output bytes.Buffer
	gin.DefaultWriter = &output
	t.Cleanup(func() { gin.DefaultWriter = previousWriter })

	router := gin.New()
	SetUpLogger(router)
	var handlerQuery string
	router.GET("/*path", func(c *gin.Context) {
		handlerQuery = c.Request.URL.RawQuery
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		target string
		logged string
	}{
		{
			target: "/v1beta/cachedContents?key=sk-secret&pageSize=2",
			logged: "/v1beta/cachedContents?key=***&pageSize=2",
		},
		{
			target: "/v1beta/models/m:streamGenerateContent?alt=sse&key=sk-secret",
			logged: "/v1beta/models/m:streamGenerateContent?alt=sse&key=***",
		},
		{
			// c.Query("key") decodes parameter names, so this is the same credential.
			target: "/v1/models?k%65y=sk-secret",
			logged: "/v1/models?k%65y=***",
		},
		{
			// gin logs the decoded path, so %3F puts a '?' ahead of the real query.
			target: "/v1beta/models/m%3Fx=y?key=sk-secret",
			logged: "/v1beta/models/m?x=y?key=***",
		},
		{
			target: "/api/status?key=plain&page=1",
			logged: "/api/status?key=plain&page=1",
		},
	}
	for _, tt := range tests {
		output.Reset()
		handlerQuery = ""
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.target, nil))

		_, rawQuery, _ := strings.Cut(tt.target, "?")
		assert.Equal(t, rawQuery, handlerQuery, "the handler must still see the original query")
		assert.Contains(t, output.String(), " "+tt.logged+"\n")
		assert.NotContains(t, output.String(), "sk-secret")
	}
}
