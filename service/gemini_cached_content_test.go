package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestFindGeminiCachedContentReference(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "no reference", body: `{"contents":[]}`},
		{name: "null reference", body: `{"cachedContent":null}`},
		{name: "reference", body: `{"cachedContent":" cachedContents/abc ","contents":[]}`, want: "cachedContents/abc"},
		{name: "snake case", body: `{"cached_content":"cachedContents/abc"}`, wantErr: "cached_content is not supported, use cachedContent"},
		{name: "duplicate key", body: `{"cachedContent":"cachedContents/a","cachedContent":"cachedContents/b"}`, wantErr: "duplicate cachedContent field"},
		{name: "not a string", body: `{"cachedContent":1}`, wantErr: "cachedContent must be a string"},
		{name: "other letter case", body: `{"CachedContent":"cachedContents/abc"}`, wantErr: "CachedContent is not supported, use cachedContent"},
		{name: "snake case in other letter case", body: `{"Cached_Content":"cachedContents/abc"}`, wantErr: "Cached_Content is not supported, use cachedContent"},
		{name: "nested batch request", body: `{"contents":[],"requests":[{"cachedContent":"cachedContents/abc"}]}`, wantErr: "cachedContent is not supported in requests"},
		{name: "nested count tokens request", body: `{"generateContentRequest":{"cachedcontent":"cachedContents/abc"}}`, wantErr: "cachedContent is not supported in generateContentRequest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindGeminiCachedContentReference([]byte(tc.body))
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseGeminiCachedContentCreateRequest(t *testing.T) {
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		body        string
		maxTTL      time.Duration
		wantModel   string
		wantTTL     time.Duration
		wantExplict bool
		wantErr     string
	}{
		{name: "default ttl", body: `{"model":"models/gemini-x"}`, maxTTL: 24 * time.Hour, wantModel: "gemini-x", wantTTL: time.Hour},
		{name: "default ttl capped by max", body: `{"model":"gemini-x"}`, maxTTL: 30 * time.Minute, wantModel: "gemini-x", wantTTL: 30 * time.Minute},
		{name: "ttl rounds up", body: `{"model":"models/gemini-x","ttl":"90.2s"}`, maxTTL: time.Hour, wantModel: "gemini-x", wantTTL: 91 * time.Second, wantExplict: true},
		{name: "expire time", body: `{"model":"models/gemini-x","expireTime":"2026-10-11T08:10:00.5Z"}`, maxTTL: time.Hour, wantModel: "gemini-x", wantTTL: 601 * time.Second, wantExplict: true},
		{name: "snake case expire time", body: `{"model":"models/gemini-x","expire_time":"2026-10-11T08:10:00Z"}`, maxTTL: time.Hour, wantModel: "gemini-x", wantTTL: 10 * time.Minute, wantExplict: true},
		{name: "ttl above max", body: `{"model":"models/gemini-x","ttl":"7200s"}`, maxTTL: time.Hour, wantErr: "ttl exceeds the maximum allowed 3600s"},
		{name: "ttl and expire time", body: `{"model":"m","ttl":"60s","expireTime":"2026-10-11T08:10:00Z"}`, maxTTL: time.Hour, wantErr: "only one of ttl or expireTime may be set"},
		{name: "both spellings", body: `{"model":"m","displayName":"a","display_name":"b"}`, maxTTL: time.Hour, wantErr: "only one of displayName or display_name may be set"},
		{name: "duplicate key", body: `{"model":"m","ttl":"60s","ttl":"86400s"}`, maxTTL: time.Hour, wantErr: "duplicate ttl field"},
		{name: "expire time in the past", body: `{"model":"m","expireTime":"2026-10-11T07:00:00Z"}`, maxTTL: time.Hour, wantErr: "expireTime must be in the future"},
		{name: "expire time beyond the duration range", body: `{"model":"m","expireTime":"2400-01-01T00:00:00Z"}`, maxTTL: time.Hour, wantErr: "ttl is too large"},
		{name: "invalid ttl", body: `{"model":"m","ttl":"60"}`, maxTTL: time.Hour, wantErr: "ttl must be a duration string such as \"3600s\""},
		{name: "missing model", body: `{"contents":[]}`, maxTTL: time.Hour, wantErr: "model is required"},
		{name: "model path traversal", body: `{"model":"models/../x"}`, maxTTL: time.Hour, wantErr: "model name must be in the format 'models/{model_name}'"},
		{name: "not an object", body: `[1]`, maxTTL: time.Hour, wantErr: "request body must be a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseGeminiCachedContentCreateRequest([]byte(tc.body), now, tc.maxTTL)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantModel, req.Model)
			assert.Equal(t, tc.wantTTL, req.TTL)
			assert.Equal(t, tc.wantExplict, req.TTLExplicit)
		})
	}
}

func TestGeminiCachedContentUpstreamPayloadKeepsClientContentVerbatim(t *testing.T) {
	body := `{"model":"models/gemini-x","display_name":"mine","ttl":"60s","usageMetadata":{"totalTokenCount":1},` +
		`"contents":[{"role":"user","parts":[{"text":"doc"}]}],"generationConfig":{"seed":12345678901234567890}}`
	req, err := ParseGeminiCachedContentCreateRequest([]byte(body), time.Now(), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, "mine", req.DisplayName)

	payload, err := req.UpstreamPayload("projects/p/locations/us-central1/publishers/google/models/gemini-x", 90*time.Second, "abc")
	require.NoError(t, err)
	parsed := gjson.ParseBytes(payload)
	assert.Equal(t, "projects/p/locations/us-central1/publishers/google/models/gemini-x", parsed.Get("model").String())
	assert.Equal(t, "90s", parsed.Get("ttl").String())
	assert.Equal(t, "gw-abc", parsed.Get("displayName").String())
	assert.False(t, parsed.Get("usageMetadata").Exists())
	assert.False(t, parsed.Get("display_name").Exists())
	assert.Equal(t, "12345678901234567890", parsed.Get("generationConfig.seed").Raw)
	assert.Equal(t, "doc", parsed.Get("contents.0.parts.0.text").String())

	// Rendering for another channel does not leak the previous model.
	again, err := req.UpstreamPayload("models/gemini-x", time.Hour, "def")
	require.NoError(t, err)
	assert.Equal(t, "models/gemini-x", gjson.GetBytes(again, "model").String())
}

func TestGeminiCachedContentTTLForChannel(t *testing.T) {
	implicit := &GeminiCachedContentCreateRequest{TTL: time.Hour}
	explicit := &GeminiCachedContentCreateRequest{TTL: time.Hour, TTLExplicit: true}

	ttl, ok := implicit.TTLForChannel(0)
	assert.True(t, ok)
	assert.Equal(t, time.Hour, ttl)

	ttl, ok = implicit.TTLForChannel(10 * time.Minute)
	assert.True(t, ok, "an implicit TTL is lowered to the gateway limit")
	assert.Equal(t, 10*time.Minute, ttl)

	_, ok = explicit.TTLForChannel(10 * time.Minute)
	assert.False(t, ok, "an explicit TTL above the gateway limit rules the channel out")
}

func TestParseGeminiCachedContentPatchRequest(t *testing.T) {
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	ttl, err := ParseGeminiCachedContentPatchRequest([]byte(`{"ttl":"7200s"}`), "ttl", now, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Hour, ttl)

	ttl, err = ParseGeminiCachedContentPatchRequest([]byte(`{"expire_time":"2026-10-11T09:00:00Z"}`), "expire_time", now, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, time.Hour, ttl)

	_, err = ParseGeminiCachedContentPatchRequest([]byte(`{"displayName":"x","ttl":"60s"}`), "", now, time.Hour)
	require.EqualError(t, err, "can not update immutable fields (model or display_name)")
	_, err = ParseGeminiCachedContentPatchRequest([]byte(`{"ttl":"60s"}`), "ttl,model", now, time.Hour)
	require.EqualError(t, err, "can not update immutable fields (model or display_name)")
	_, err = ParseGeminiCachedContentPatchRequest([]byte(`{}`), "", now, time.Hour)
	require.EqualError(t, err, "ttl or expireTime is required")
	_, err = ParseGeminiCachedContentPatchRequest([]byte(`{"ttl":"7200s"}`), "", now, time.Hour)
	require.EqualError(t, err, "ttl exceeds the maximum allowed 3600s")
}

func TestIsGeminiCachedContentNotFound(t *testing.T) {
	cases := []struct {
		status int
		text   string
		want   bool
	}{
		{http.StatusForbidden, `{"error":{"code":403,"message":"CachedContent not found (or permission denied)","status":"PERMISSION_DENIED"}}`, true},
		{http.StatusBadRequest, "Invalid resource state for cache content 123.", true},
		{http.StatusNotFound, "Cached content 123 is not found.", true},
		{http.StatusForbidden, "Permission denied on resource project p.", false},
		{http.StatusBadRequest, "Invalid argument.", false},
		{http.StatusTooManyRequests, "CachedContent not found (or permission denied)", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsGeminiCachedContentNotFound(tc.status, tc.text), "%d %s", tc.status, tc.text)
	}
}

func TestGeminiCachedContentPublicID(t *testing.T) {
	id, err := NewGeminiCachedContentPublicID()
	require.NoError(t, err)
	assert.Len(t, id, 40)

	parsed, ok := ParseGeminiCachedContentPublicID(GeminiCachedContentNamePrefix + id)
	assert.True(t, ok)
	assert.Equal(t, id, parsed)

	for _, name := range []string{"", "cachedContents/", "cachedContents/ABC", "cachedContents/a-b", "projects/p/locations/l/cachedContents/1"} {
		_, ok := ParseGeminiCachedContentPublicID(name)
		assert.False(t, ok, name)
	}
}

func TestGoogleErrorStatus(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusBadRequest:            "INVALID_ARGUMENT",
		http.StatusUnauthorized:          "UNAUTHENTICATED",
		http.StatusForbidden:             "PERMISSION_DENIED",
		http.StatusNotFound:              "NOT_FOUND",
		http.StatusRequestEntityTooLarge: "UNKNOWN",
		http.StatusTooManyRequests:       "RESOURCE_EXHAUSTED",
		499:                              "CANCELLED",
		http.StatusInternalServerError:   "INTERNAL",
		http.StatusNotImplemented:        "UNIMPLEMENTED",
		http.StatusBadGateway:            "UNAVAILABLE",
		http.StatusServiceUnavailable:    "UNAVAILABLE",
		http.StatusGatewayTimeout:        "DEADLINE_EXCEEDED",
	} {
		assert.Equal(t, want, GoogleErrorStatus(status), status)
	}
	assert.JSONEq(t, `{"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}`, string(GoogleErrorBody(http.StatusServiceUnavailable, "busy")))
}
