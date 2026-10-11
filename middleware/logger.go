package middleware

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

// loggedQueryParamPattern matches one query parameter of a logged request path.
// gin logs the decoded URL path ahead of the raw query, so a '?' sent as %3F in
// the path can precede the real query; '?' therefore separates parameters as
// well as '&'.
var loggedQueryParamPattern = regexp.MustCompile(`[^?&]+`)

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(redactTaskArtifactAccessQuery())
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		path := param.Path
		// OAuth callbacks carry one-time codes and state in the query string.
		// Redact the log value only; the handler still needs the original query.
		if strings.HasPrefix(path, "/api/oauth/") || strings.HasPrefix(path, "/oauth/") {
			path, _, _ = strings.Cut(path, "?")
		}
		// Gemini clients may send the gateway token as ?key= on the routes where
		// TokenAuth accepts it. Mask it in the log value only; the request keeps
		// its original query.
		if strings.HasPrefix(path, "/v1beta/models") ||
			strings.HasPrefix(path, "/v1beta/cachedContents") ||
			strings.HasPrefix(path, "/v1beta/openai/models") ||
			strings.HasPrefix(path, "/v1/models") {
			path = maskQueryAPIKey(path)
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			path,
		)
	}))
}

// maskQueryAPIKey replaces the value of every `key` query parameter in a logged
// request path with a fixed mask and keeps the rest of the path as logged.
func maskQueryAPIKey(path string) string {
	route, query, hasQuery := strings.Cut(path, "?")
	if !hasQuery {
		return path
	}
	return route + "?" + loggedQueryParamPattern.ReplaceAllStringFunc(query, func(param string) string {
		name, value, _ := strings.Cut(param, "=")
		// Decode the name the way c.Query("key") does, so k%65y is caught too.
		if decoded, err := url.QueryUnescape(name); err != nil || decoded != "key" || value == "" {
			return param
		}
		return name + "=***"
	})
}
