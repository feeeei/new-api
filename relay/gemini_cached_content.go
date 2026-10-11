package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// An upstream create or update keeps running after the client disconnects,
	// so the gateway can still record, bill or clean up what upstream did.
	geminiCachedContentUpstreamTimeout  = 5 * time.Minute
	geminiCachedContentResponseMaxBytes = 4 << 20
	geminiCachedContentOfficialBaseURL  = "https://generativelanguage.googleapis.com"
)

var (
	geminiCachedContentAIStudioName = regexp.MustCompile(`^cachedContents/[A-Za-z0-9_-]+$`)
	geminiCachedContentVertexName   = regexp.MustCompile(`^projects/[A-Za-z0-9_.:-]+/locations/([a-z0-9-]+)/cachedContents/[A-Za-z0-9_-]+$`)
	vertexCachedContentProject      = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	vertexCachedContentRegion       = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// GeminiCachedContentUpstream is the upstream cache resource a call returned.
type GeminiCachedContentUpstream struct {
	Name            string
	TotalTokenCount int64
	// ExpireTime is zero when upstream did not report a usable expiry.
	ExpireTime time.Time
}

// GeminiCachedContentUpstreamError is a non-2xx answer of the upstream
// cachedContents API.
type GeminiCachedContentUpstreamError struct {
	StatusCode int
	Body       []byte
	// Gateway marks an answer from a declared gateway channel, whose rate
	// limits and outages reflect the upstream credential holding the cache.
	Gateway bool
}

func (e *GeminiCachedContentUpstreamError) Error() string {
	return fmt.Sprintf("gemini cached content upstream returned status %d", e.StatusCode)
}

// NotFound reports that upstream does not have the cache (missing, expired,
// deleted or held by another credential).
func (e *GeminiCachedContentUpstreamError) NotFound() bool {
	return e != nil && service.IsGeminiCachedContentNotFound(e.StatusCode, string(e.Body))
}

// CredentialRejected reports that upstream refused the channel credential
// itself. AI Studio answers an invalid or expired API key with 400.
func (e *GeminiCachedContentUpstreamError) CredentialRejected() bool {
	if e == nil || e.NotFound() {
		return false
	}
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		body := strings.ToLower(string(e.Body))
		return strings.Contains(body, "api_key_invalid") || strings.Contains(body, "api_key_expired") ||
			strings.Contains(body, "api key not valid") || strings.Contains(body, "api key expired")
	default:
		return false
	}
}

// RetryableOnOtherChannel reports a credential, quota or availability failure
// that another channel's credential may not share.
func (e *GeminiCachedContentUpstreamError) RetryableOnOtherChannel() bool {
	if e == nil || e.NotFound() {
		return false
	}
	return e.CredentialRejected() || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= http.StatusInternalServerError
}

// GeminiCachedContentMalformedResponseError is a 2xx answer the gateway cannot
// use. Name is set when the created resource is known, so it can be deleted.
type GeminiCachedContentMalformedResponseError struct {
	Name string
	Err  error
}

func (e *GeminiCachedContentMalformedResponseError) Error() string {
	return "gemini cached content upstream returned an unusable response: " + e.Err.Error()
}

func (e *GeminiCachedContentMalformedResponseError) Unwrap() error {
	return e.Err
}

// GeminiCachedContentUpstreamModel returns the model resource name the
// channel's cachedContents API expects for info.UpstreamModelName.
func GeminiCachedContentUpstreamModel(info *relaycommon.RelayInfo) (string, error) {
	switch info.ChannelType {
	case constant.ChannelTypeGemini:
		return "models/" + info.UpstreamModelName, nil
	case constant.ChannelTypeVertexAi:
		project, region, err := vertexCachedContentLocation(info)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", project, region, info.UpstreamModelName), nil
	default:
		return "", fmt.Errorf("channel type %d does not support explicit context caching", info.ChannelType)
	}
}

// CreateGeminiCachedContentUpstream creates a cache on the channel in info;
// payload is the complete upstream request body.
func CreateGeminiCachedContentUpstream(c *gin.Context, info *relaycommon.RelayInfo, payload []byte) (*GeminiCachedContentUpstream, error) {
	var target string
	switch info.ChannelType {
	case constant.ChannelTypeGemini:
		target = geminiCachedContentAPIBase(info) + "/v1beta/cachedContents"
	case constant.ChannelTypeVertexAi:
		project, region, err := vertexCachedContentLocation(info)
		if err != nil {
			return nil, err
		}
		target = vertex.BuildAPIBaseURL(info.ChannelBaseUrl, vertex.DefaultAPIVersion, project, region) + "/cachedContents"
	default:
		return nil, fmt.Errorf("channel type %d does not support explicit context caching", info.ChannelType)
	}
	body, err := doGeminiCachedContentRequest(c, info, http.MethodPost, target, payload)
	if err != nil {
		return nil, err
	}

	// Later updates and deletes address the resource by this name, so only a
	// name the channel can address again is usable.
	name := strings.TrimSpace(gjson.GetBytes(body, "name").String())
	if _, err := geminiCachedContentResourceURL(info, name); err != nil {
		return nil, &GeminiCachedContentMalformedResponseError{Err: err}
	}
	tokenCount := gjson.GetBytes(body, "usageMetadata.totalTokenCount")
	tokens := tokenCount.Int()
	if (tokenCount.Type != gjson.Number && tokenCount.Type != gjson.String) || tokens <= 0 || tokens > math.MaxInt32 {
		return nil, &GeminiCachedContentMalformedResponseError{Name: name, Err: fmt.Errorf("invalid usageMetadata.totalTokenCount %q", tokenCount.Raw)}
	}
	return &GeminiCachedContentUpstream{Name: name, TotalTokenCount: tokens, ExpireTime: parseGeminiCachedContentExpireTime(body)}, nil
}

// PatchGeminiCachedContentUpstreamTTL moves the upstream cache's expiry to ttl
// from now.
func PatchGeminiCachedContentUpstreamTTL(c *gin.Context, info *relaycommon.RelayInfo, upstreamName string, ttl time.Duration) (*GeminiCachedContentUpstream, error) {
	target, err := geminiCachedContentResourceURL(info, upstreamName)
	if err != nil {
		return nil, err
	}
	payload, err := common.Marshal(map[string]string{"ttl": service.GeminiCachedContentTTLString(ttl)})
	if err != nil {
		return nil, err
	}
	body, err := doGeminiCachedContentRequest(c, info, http.MethodPatch, target+"?updateMask=ttl", payload)
	if err != nil {
		return nil, err
	}
	return &GeminiCachedContentUpstream{Name: upstreamName, ExpireTime: parseGeminiCachedContentExpireTime(body)}, nil
}

// DeleteGeminiCachedContentUpstream deletes the upstream cache.
func DeleteGeminiCachedContentUpstream(c *gin.Context, info *relaycommon.RelayInfo, upstreamName string) error {
	target, err := geminiCachedContentResourceURL(info, upstreamName)
	if err != nil {
		return err
	}
	_, err = doGeminiCachedContentRequest(c, info, http.MethodDelete, target, nil)
	return err
}

// doGeminiCachedContentRequest sends one authenticated cachedContents call and
// returns the 2xx response body.
func doGeminiCachedContentRequest(c *gin.Context, info *relaycommon.RelayInfo, method, target string, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), geminiCachedContentUpstreamTimeout)
	defer cancel()
	var requestBody io.Reader
	if payload != nil {
		requestBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, requestBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch info.ChannelType {
	case constant.ChannelTypeGemini:
		req.Header.Set("x-goog-api-key", info.ApiKey)
	case constant.ChannelTypeVertexAi:
		project, _, err := vertexCachedContentLocation(info)
		if err != nil {
			return nil, err
		}
		accessToken, err := vertex.ServiceAccountAccessToken(info)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("x-goog-user-project", project)
	default:
		return nil, fmt.Errorf("channel type %d does not support explicit context caching", info.ChannelType)
	}
	// Channel header overrides apply after authentication, as on relay requests.
	headerOverride, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range headerOverride {
		req.Header.Set(key, value)
		if strings.EqualFold(key, "Host") {
			req.Host = value
		}
	}

	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	// Clients are shared; never follow a redirect that would carry the
	// credential headers to another host.
	upstreamClient := *client
	upstreamClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, geminiCachedContentResponseMaxBytes))
	success := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if err != nil {
		if success {
			return nil, &GeminiCachedContentMalformedResponseError{Err: err}
		}
		return nil, err
	}
	if !success {
		return nil, &GeminiCachedContentUpstreamError{StatusCode: resp.StatusCode, Body: body, Gateway: isGeminiExplicitCacheGateway(info)}
	}
	return body, nil
}

// isGeminiExplicitCacheGateway reports whether the channel in info is a
// declared gateway: its rate limits and outages concern the upstream
// credential holding the cache, not the channel itself.
func isGeminiExplicitCacheGateway(info *relaycommon.RelayInfo) bool {
	return info.ChannelType == constant.ChannelTypeGemini &&
		info.ChannelOtherSettings.GeminiExplicitCacheUpstream &&
		!model.IsOfficialGeminiBaseURL(info.ChannelBaseUrl)
}

// geminiCachedContentResourceURL returns the URL of an upstream cache the
// channel in info can address, rejecting names of any other shape.
func geminiCachedContentResourceURL(info *relaycommon.RelayInfo, upstreamName string) (string, error) {
	switch info.ChannelType {
	case constant.ChannelTypeGemini:
		if !geminiCachedContentAIStudioName.MatchString(upstreamName) {
			return "", fmt.Errorf("invalid upstream cached content name %q", upstreamName)
		}
		return geminiCachedContentAPIBase(info) + "/v1beta/" + upstreamName, nil
	case constant.ChannelTypeVertexAi:
		match := geminiCachedContentVertexName.FindStringSubmatch(upstreamName)
		if match == nil {
			return "", fmt.Errorf("invalid upstream cached content name %q", upstreamName)
		}
		return vertex.BuildAPIBaseURL(info.ChannelBaseUrl, vertex.DefaultAPIVersion, "", match[1]) + "/" + upstreamName, nil
	default:
		return "", fmt.Errorf("channel type %d does not support explicit context caching", info.ChannelType)
	}
}

func geminiCachedContentAPIBase(info *relaycommon.RelayInfo) string {
	base := strings.TrimRight(strings.TrimSpace(info.ChannelBaseUrl), "/")
	if base == "" {
		return geminiCachedContentOfficialBaseURL
	}
	return base
}

// vertexCachedContentLocation returns the service account's project and the
// region the relay uses for the model. Express-mode API keys cannot call the
// cachedContents resource.
func vertexCachedContentLocation(info *relaycommon.RelayInfo) (string, string, error) {
	if info.ChannelOtherSettings.VertexKeyType == dto.VertexKeyTypeAPIKey {
		return "", "", errors.New("vertex api key channels do not support explicit context caching")
	}
	credentials := vertex.Credentials{}
	if err := common.UnmarshalJsonStr(info.ApiKey, &credentials); err != nil {
		return "", "", fmt.Errorf("failed to decode credentials file: %w", err)
	}
	project := strings.TrimSpace(credentials.ProjectID)
	if !vertexCachedContentProject.MatchString(project) {
		return "", "", fmt.Errorf("invalid vertex project_id %q", project)
	}
	region := strings.TrimSpace(vertex.GetModelRegion(info.ApiVersion, info.OriginModelName))
	if region == "" {
		region = "global"
	}
	if !vertexCachedContentRegion.MatchString(region) {
		return "", "", fmt.Errorf("invalid vertex region %q", region)
	}
	return project, region, nil
}

func parseGeminiCachedContentExpireTime(body []byte) time.Time {
	expireTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(gjson.GetBytes(body, "expireTime").String()))
	if err != nil {
		return time.Time{}
	}
	return expireTime
}
