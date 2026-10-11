package service

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

const (
	// GeminiCachedContentNamePrefix prefixes cache resource names on the wire.
	GeminiCachedContentNamePrefix = "cachedContents/"
	// GeminiCachedContentNotFoundMessage is Google's text for a missing, expired
	// or foreign cache; gateways answer with the same 403 so clients see one shape.
	GeminiCachedContentNotFoundMessage = "CachedContent not found (or permission denied)"

	GeminiCachedContentListDefaultPageSize = 50
	GeminiCachedContentListMaxPageSize     = 1000

	geminiCachedContentDefaultTTL          = time.Hour
	geminiCachedContentDisplayNameMaxRunes = 128
	geminiCachedContentModelMaxLength      = 255
	// The upstream resource carries a neutral tag instead of the client's
	// display name, which stays local to the gateway.
	geminiCachedContentUpstreamDisplayNamePrefix = "gw-"

	geminiCachedContentBindingKey = "gemini_cached_content_binding"
	geminiRawErrorKey             = "gemini_raw_error"
)

// GeminiCachedContentNotFoundBody is the exact 403 body for a missing cache.
var GeminiCachedContentNotFoundBody = []byte(`{"error":{"code":403,"message":"CachedContent not found (or permission denied)","status":"PERMISSION_DENIED"}}`)

var (
	ErrGeminiCachedContentNotFound = errors.New("gemini cached content not found")

	geminiCachedContentPublicIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)
	geminiCachedContentIDEncoding      = base32.StdEncoding.WithPadding(base32.NoPadding)

	// geminiCachedContentManagedKeys are set by the gateway, never forwarded from the client.
	geminiCachedContentManagedKeys = []string{"name", "model", "displayName", "ttl", "expireTime", "createTime", "updateTime", "usageMetadata"}
	// geminiCachedContentFieldAliases maps accepted snake_case metadata keys to camelCase.
	geminiCachedContentFieldAliases = [][2]string{
		{"display_name", "displayName"},
		{"expire_time", "expireTime"},
		{"create_time", "createTime"},
		{"update_time", "updateTime"},
		{"usage_metadata", "usageMetadata"},
	}
)

// NewGeminiCachedContentPublicID returns a 40 character lowercase id.
func NewGeminiCachedContentPublicID() (string, error) {
	var b [25]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(geminiCachedContentIDEncoding.EncodeToString(b[:])), nil
}

// ParseGeminiCachedContentPublicID accepts "cachedContents/{id}" or a bare id.
func ParseGeminiCachedContentPublicID(name string) (string, bool) {
	id := strings.TrimPrefix(strings.TrimSpace(name), GeminiCachedContentNamePrefix)
	if !geminiCachedContentPublicIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// GeminiCachedContentView is the client-visible cache metadata.
func GeminiCachedContentView(record *model.GeminiCachedContent) map[string]any {
	view := map[string]any{
		"name":          GeminiCachedContentNamePrefix + record.PublicId,
		"model":         "models/" + record.Model,
		"usageMetadata": map[string]any{"totalTokenCount": record.TotalTokenCount},
		"createTime":    time.Unix(record.CreatedTime, 0).UTC().Format(time.RFC3339Nano),
		"updateTime":    time.Unix(record.UpdatedTime, 0).UTC().Format(time.RFC3339Nano),
		"expireTime":    time.Unix(record.ExpireTime, 0).UTC().Format(time.RFC3339Nano),
	}
	if record.DisplayName != "" {
		view["displayName"] = record.DisplayName
	}
	return view
}

// GeminiCachedContentStorageTokenHours is the storage quantity billed as cs.
func GeminiCachedContentStorageTokenHours(tokens int64, duration time.Duration) float64 {
	if tokens <= 0 || duration <= 0 {
		return 0
	}
	return float64(tokens) * duration.Hours()
}

// ClampGeminiCachedContentExpire returns the expiry the gateway records and
// bills: the upstream value, never later than limit, or limit when missing.
func ClampGeminiCachedContentExpire(upstream, limit time.Time) time.Time {
	if upstream.IsZero() || upstream.After(limit) {
		return limit
	}
	return upstream
}

// IsGeminiCachedContentNotFound recognizes upstream answers meaning the
// referenced cache does not exist (missing, expired, deleted or foreign):
// AI Studio and gateways answer 403, Vertex generation 400 and Vertex cache
// resources 404. text is the response body or its error message.
func IsGeminiCachedContentNotFound(statusCode int, text string) bool {
	lower := strings.ToLower(text)
	switch statusCode {
	case http.StatusForbidden:
		return strings.Contains(lower, "cachedcontent not found")
	case http.StatusBadRequest:
		return strings.Contains(lower, "invalid resource state for cache content")
	case http.StatusNotFound:
		return strings.Contains(lower, "cached content") && strings.Contains(lower, "not found")
	default:
		return false
	}
}

// GoogleErrorStatus maps an HTTP status to a Google API error status.
func GoogleErrorStatus(httpStatus int) string {
	switch httpStatus {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case http.StatusNotImplemented:
		return "UNIMPLEMENTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		if httpStatus >= http.StatusInternalServerError {
			return "INTERNAL"
		}
		return "UNKNOWN"
	}
}

// GoogleErrorBody renders a Google API error body.
func GoogleErrorBody(httpStatus int, message string) []byte {
	body, err := common.Marshal(map[string]any{
		"error": map[string]any{
			"code":    httpStatus,
			"message": message,
			"status":  GoogleErrorStatus(httpStatus),
		},
	})
	if err != nil {
		return []byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`)
	}
	return body
}

// AbortWithGoogleError writes a Google API error and aborts the request.
func AbortWithGoogleError(c *gin.Context, httpStatus int, message string) {
	c.Data(httpStatus, "application/json", GoogleErrorBody(httpStatus, message))
	c.Abort()
}

// AbortWithGeminiCachedContentNotFound writes the standard missing-cache 403.
func AbortWithGeminiCachedContentNotFound(c *gin.Context) {
	c.Data(http.StatusForbidden, "application/json", GeminiCachedContentNotFoundBody)
	c.Abort()
}

// GeminiRawError is an error response a Gemini-format relay returns verbatim
// instead of rendering its own error body.
type GeminiRawError struct {
	StatusCode int
	Body       []byte
}

func SetGeminiRawError(c *gin.Context, statusCode int, body []byte) {
	c.Set(geminiRawErrorKey, &GeminiRawError{StatusCode: statusCode, Body: body})
}

func GetGeminiRawError(c *gin.Context) (*GeminiRawError, bool) {
	value, ok := c.Get(geminiRawErrorKey)
	if !ok {
		return nil, false
	}
	rawError, ok := value.(*GeminiRawError)
	return rawError, ok && rawError != nil
}

// FindGeminiCachedContentReference returns the cache name a generateContent
// request references, or "" when it references none. Any other spelling of the
// key (JSON decoding ignores case), a duplicate key, a non-string value or a
// reference nested in requests or generateContentRequest is rejected, so the
// reference checked here is the only one that can reach upstream.
func FindGeminiCachedContentReference(body []byte) (string, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return "", nil
	}
	name := ""
	seen := false
	var err error
	root.ForEach(func(key, value gjson.Result) bool {
		switch spelling := key.String(); {
		case spelling == "cachedContent":
			if seen {
				err = errors.New("duplicate cachedContent field")
				break
			}
			seen = true
			switch value.Type {
			case gjson.Null:
			case gjson.String:
				name = strings.TrimSpace(value.String())
			default:
				err = errors.New("cachedContent must be a string")
			}
		case isGeminiCachedContentKey(spelling):
			err = fmt.Errorf("%s is not supported, use cachedContent", spelling)
		case spelling == "requests" || spelling == "generateContentRequest":
			nested := []gjson.Result{value}
			if value.IsArray() {
				nested = value.Array()
			}
			for _, request := range nested {
				request.ForEach(func(nestedKey, _ gjson.Result) bool {
					if isGeminiCachedContentKey(nestedKey.String()) {
						err = fmt.Errorf("cachedContent is not supported in %s", spelling)
					}
					return err == nil
				})
			}
		}
		return err == nil
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

func isGeminiCachedContentKey(key string) bool {
	return strings.EqualFold(key, "cachedContent") || strings.EqualFold(key, "cached_content")
}

// RewriteGeminiCachedContentReference points the request at the upstream resource.
func RewriteGeminiCachedContentReference(body []byte, upstreamName string) ([]byte, error) {
	return sjson.SetBytes(body, "cachedContent", upstreamName)
}

// GeminiCachedContentCreateRequest is a validated create request.
type GeminiCachedContentCreateRequest struct {
	// Model is the client-facing model without the "models/" prefix.
	Model       string
	TTL         time.Duration
	TTLExplicit bool
	DisplayName string
	// payload is the client JSON without gateway-managed keys, numbers verbatim.
	payload []byte
}

// ParseGeminiCachedContentCreateRequest validates a create request. Without
// ttl or expireTime the cache lives one hour, capped by maxTTL.
func ParseGeminiCachedContentCreateRequest(body []byte, now time.Time, maxTTL time.Duration) (*GeminiCachedContentCreateRequest, error) {
	payload, err := normalizeGeminiCachedContentPayload(body)
	if err != nil {
		return nil, err
	}
	modelValue := gjson.GetBytes(payload, "model")
	model := ""
	if modelValue.Type == gjson.String {
		model = strings.TrimPrefix(strings.TrimSpace(modelValue.String()), "models/")
	}
	if model == "" {
		return nil, errors.New("model is required")
	}
	if !isSafeGeminiModelSegment(model) {
		return nil, errors.New("model name must be in the format 'models/{model_name}'")
	}

	ttl, err := parseGeminiCachedContentExpiration(payload, now)
	if err != nil {
		return nil, err
	}
	explicit := ttl > 0
	if !explicit {
		ttl = geminiCachedContentDefaultTTL
		if maxTTL > 0 && ttl > maxTTL {
			ttl = maxTTL
		}
	}
	if maxTTL > 0 && ttl > maxTTL {
		return nil, fmt.Errorf("ttl exceeds the maximum allowed %ds", int64(maxTTL.Seconds()))
	}

	displayName := ""
	if value := gjson.GetBytes(payload, "displayName"); value.Exists() && value.Type != gjson.Null {
		if value.Type != gjson.String {
			return nil, errors.New("displayName must be a string")
		}
		displayName = value.String()
		if utf8.RuneCountInString(displayName) > geminiCachedContentDisplayNameMaxRunes {
			return nil, fmt.Errorf("displayName must be at most %d characters", geminiCachedContentDisplayNameMaxRunes)
		}
	}

	for _, key := range geminiCachedContentManagedKeys {
		if payload, err = sjson.DeleteBytes(payload, key); err != nil {
			return nil, errors.New("request body must be a JSON object")
		}
	}
	return &GeminiCachedContentCreateRequest{
		Model:       model,
		TTL:         ttl,
		TTLExplicit: explicit,
		DisplayName: displayName,
		payload:     payload,
	}, nil
}

// TTLForChannel returns the TTL to create the cache with on a channel whose
// gateway limits TTL to channelMaxTTL (zero: no limit). An implicit TTL is
// lowered to the limit; an explicit one above it rules the channel out.
func (r *GeminiCachedContentCreateRequest) TTLForChannel(channelMaxTTL time.Duration) (time.Duration, bool) {
	if channelMaxTTL <= 0 || r.TTL <= channelMaxTTL {
		return r.TTL, true
	}
	if r.TTLExplicit {
		return 0, false
	}
	return channelMaxTTL, true
}

// UpstreamPayload renders the body sent to the upstream cachedContents API.
func (r *GeminiCachedContentCreateRequest) UpstreamPayload(upstreamModel string, ttl time.Duration, publicID string) ([]byte, error) {
	out := append([]byte(nil), r.payload...)
	var err error
	if out, err = sjson.SetBytes(out, "model", upstreamModel); err != nil {
		return nil, err
	}
	if out, err = sjson.SetBytes(out, "ttl", GeminiCachedContentTTLString(ttl)); err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "displayName", geminiCachedContentUpstreamDisplayNamePrefix+publicID)
}

// TokenCountRequest returns the cache contents shaped as a generateContent
// request for local token estimation and sensitive-word checks.
func (r *GeminiCachedContentCreateRequest) TokenCountRequest() (*dto.GeminiChatRequest, error) {
	request := &dto.GeminiChatRequest{}
	if err := common.Unmarshal(r.payload, request); err != nil {
		return nil, err
	}
	return request, nil
}

// GeminiCachedContentTTLString renders whole seconds as a Google duration.
func GeminiCachedContentTTLString(ttl time.Duration) string {
	return strconv.FormatInt(int64(math.Ceil(ttl.Seconds())), 10) + "s"
}

// ParseGeminiCachedContentPatchRequest validates an update, which may only
// change the expiration, and returns the new TTL counted from now.
func ParseGeminiCachedContentPatchRequest(body []byte, updateMask string, now time.Time, maxTTL time.Duration) (time.Duration, error) {
	payload, err := normalizeGeminiCachedContentPayload(body)
	if err != nil {
		return 0, err
	}
	gjson.ParseBytes(payload).ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "ttl", "expireTime", "name":
		default:
			err = errors.New("can not update immutable fields (model or display_name)")
		}
		return err == nil
	})
	if err != nil {
		return 0, err
	}
	for field := range strings.SplitSeq(updateMask, ",") {
		switch strings.TrimSpace(field) {
		case "", "ttl", "expireTime", "expire_time":
		default:
			return 0, errors.New("can not update immutable fields (model or display_name)")
		}
	}
	ttl, err := parseGeminiCachedContentExpiration(payload, now)
	if err != nil {
		return 0, err
	}
	if ttl == 0 {
		return 0, errors.New("ttl or expireTime is required")
	}
	if maxTTL > 0 && ttl > maxTTL {
		return 0, fmt.Errorf("ttl exceeds the maximum allowed %ds", int64(maxTTL.Seconds()))
	}
	return ttl, nil
}

// normalizeGeminiCachedContentPayload checks the body is one JSON object with
// unique top-level keys and renames snake_case metadata keys to camelCase.
// Unique keys guarantee the values validated here are the ones forwarded.
func normalizeGeminiCachedContentPayload(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("request body must be a JSON object")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, errors.New("request body must be a JSON object")
	}
	seen := make(map[string]bool)
	var err error
	root.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if seen[name] {
			err = fmt.Errorf("duplicate %s field", name)
			return false
		}
		seen[name] = true
		return true
	})
	if err != nil {
		return nil, err
	}
	payload := []byte(root.Raw)
	for _, alias := range geminiCachedContentFieldAliases {
		snake, camel := alias[0], alias[1]
		if !seen[snake] {
			continue
		}
		if seen[camel] {
			return nil, fmt.Errorf("only one of %s or %s may be set", camel, snake)
		}
		raw := gjson.GetBytes(payload, snake).Raw
		if payload, err = sjson.DeleteBytes(payload, snake); err != nil {
			return nil, errors.New("request body must be a JSON object")
		}
		if payload, err = sjson.SetRawBytes(payload, camel, []byte(raw)); err != nil {
			return nil, errors.New("request body must be a JSON object")
		}
	}
	return payload, nil
}

// parseGeminiCachedContentExpiration reads ttl or expireTime (at most one),
// returning zero when neither is set. The result rounds up to whole seconds,
// matching the ttl sent upstream.
func parseGeminiCachedContentExpiration(payload []byte, now time.Time) (time.Duration, error) {
	ttlValue := gjson.GetBytes(payload, "ttl")
	expireValue := gjson.GetBytes(payload, "expireTime")
	hasTTL := ttlValue.Exists() && ttlValue.Type != gjson.Null
	hasExpire := expireValue.Exists() && expireValue.Type != gjson.Null
	if hasTTL && hasExpire {
		return 0, errors.New("only one of ttl or expireTime may be set")
	}
	if hasTTL {
		if ttlValue.Type != gjson.String {
			return 0, errors.New("ttl must be a duration string such as \"3600s\"")
		}
		return parseGoogleDurationSeconds(ttlValue.String())
	}
	if hasExpire {
		if expireValue.Type != gjson.String {
			return 0, errors.New("expireTime must be an RFC 3339 timestamp")
		}
		expire, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(expireValue.String()))
		if err != nil {
			return 0, errors.New("expireTime must be an RFC 3339 timestamp")
		}
		seconds := math.Ceil(expire.Sub(now).Seconds())
		if seconds <= 0 {
			return 0, errors.New("expireTime must be in the future")
		}
		if seconds > float64(math.MaxInt64/int64(time.Second)) {
			return 0, errors.New("ttl is too large")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	return 0, nil
}

func parseGoogleDurationSeconds(raw string) (time.Duration, error) {
	number, ok := strings.CutSuffix(strings.TrimSpace(raw), "s")
	if !ok {
		return 0, errors.New("ttl must be a duration string such as \"3600s\"")
	}
	seconds, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, errors.New("ttl must be a duration string such as \"3600s\"")
	}
	if seconds <= 0 {
		return 0, errors.New("ttl must be positive")
	}
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, errors.New("ttl is too large")
	}
	return time.Duration(math.Ceil(seconds)) * time.Second, nil
}

func isSafeGeminiModelSegment(model string) bool {
	if len(model) > geminiCachedContentModelMaxLength {
		return false
	}
	dotsOnly := true
	for i := range len(model) {
		b := model[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-':
			dotsOnly = false
		case b == '.':
		default:
			return false
		}
	}
	return !dotsOnly
}

// GeminiCachedContentUsableGroups returns the groups whose caches the current
// token may use: its own group, or its auto groups for an auto token.
func GeminiCachedContentUsableGroups(c *gin.Context) []string {
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if usingGroup != "auto" {
		return []string{usingGroup}
	}
	return GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
}

// LookupGeminiCachedContent returns the current token's live cache named name.
// A malformed, missing, expired or foreign cache is ErrGeminiCachedContentNotFound.
func LookupGeminiCachedContent(c *gin.Context, name string) (*model.GeminiCachedContent, error) {
	publicID, ok := ParseGeminiCachedContentPublicID(name)
	if !ok {
		return nil, ErrGeminiCachedContentNotFound
	}
	record, err := model.GetGeminiCachedContentForToken(common.GetContextKeyInt(c, constant.ContextKeyTokenId), publicID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrGeminiCachedContentNotFound
	}
	if err != nil {
		return nil, err
	}
	if record.ExpireTime <= time.Now().Unix() || !slices.Contains(GeminiCachedContentUsableGroups(c), record.UsingGroup) {
		return nil, ErrGeminiCachedContentNotFound
	}
	return record, nil
}

// GeminiCachedContentBinding ties a generation request to the cache it references.
type GeminiCachedContentBinding struct {
	Record *model.GeminiCachedContent
}

// GeminiCachedContentError is a rejection to return in Google error format.
type GeminiCachedContentError struct {
	StatusCode int
	Message    string
}

// Body renders the error; a missing cache uses the standard 403 body.
func (e *GeminiCachedContentError) Body() []byte {
	if e.StatusCode == http.StatusForbidden && e.Message == GeminiCachedContentNotFoundMessage {
		return GeminiCachedContentNotFoundBody
	}
	return GoogleErrorBody(e.StatusCode, e.Message)
}

// ResolveGeminiCachedContentBinding binds a Gemini generation request that
// references an explicit cache to the channel credential holding it: it pins
// the channel without retries and records the binding for key selection and
// body rewriting. It returns (nil, nil) when no cache is referenced.
func ResolveGeminiCachedContentBinding(c *gin.Context, requestModel string) (*GeminiCachedContentBinding, *GeminiCachedContentError) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, nil
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, nil
	}
	name, err := FindGeminiCachedContentReference(body)
	if err != nil {
		return nil, &GeminiCachedContentError{StatusCode: http.StatusBadRequest, Message: err.Error()}
	}
	if name == "" {
		return nil, nil
	}
	record, err := LookupGeminiCachedContent(c, name)
	if errors.Is(err, ErrGeminiCachedContentNotFound) {
		return nil, &GeminiCachedContentError{StatusCode: http.StatusForbidden, Message: GeminiCachedContentNotFoundMessage}
	}
	if err != nil {
		common.SysError(common.LogText("failed to load gemini cached content: %s", err.Error()))
		return nil, &GeminiCachedContentError{StatusCode: http.StatusInternalServerError, Message: i18n.T(c, i18n.MsgGeminiCacheLookupFailed)}
	}
	if record.Model != requestModel {
		// Google's own validation text, kept verbatim like the other request checks.
		return nil, &GeminiCachedContentError{StatusCode: http.StatusBadRequest, Message: fmt.Sprintf("Model used by GenerateContent request (models/%s) and CachedContent (models/%s) has to be the same.", requestModel, record.Model)}
	}
	unavailable := &GeminiCachedContentError{StatusCode: http.StatusServiceUnavailable, Message: i18n.T(c, i18n.MsgGeminiCacheChannelUnavailable)}
	channel, err := model.CacheGetChannel(record.ChannelId)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled || !channel.SupportsGeminiExplicitCache() {
		return nil, unavailable
	}
	if !model.IsChannelEnabledForGroupModel(record.UsingGroup, record.Model, record.ChannelId) {
		return nil, unavailable
	}
	binding := &GeminiCachedContentBinding{Record: record}
	if _, ok := binding.KeyFor(channel); !ok {
		return nil, unavailable
	}
	GetChannelConstraints(c).AddPin(taskdto.ChannelPin{
		ChannelId: record.ChannelId,
		Source:    taskdto.PinSourceGeminiCachedContent,
		Rank:      taskdto.PinRankGeminiCachedContent,
		RetryMode: taskdto.PinRetrySingleAttempt,
	})
	if common.GetContextKeyString(c, constant.ContextKeyUsingGroup) == "auto" {
		common.SetContextKey(c, constant.ContextKeyAutoGroup, record.UsingGroup)
	}
	SetGeminiCachedContentBinding(c, binding)
	return binding, nil
}

// SetGeminiCachedContentBinding records that the request operates on a bound
// cache, so channel setup selects the credential holding it.
func SetGeminiCachedContentBinding(c *gin.Context, binding *GeminiCachedContentBinding) {
	c.Set(geminiCachedContentBindingKey, binding)
}

// GetGeminiCachedContentBinding returns the request's cache binding, if any.
func GetGeminiCachedContentBinding(c *gin.Context) *GeminiCachedContentBinding {
	value, ok := c.Get(geminiCachedContentBindingKey)
	if !ok {
		return nil
	}
	binding, _ := value.(*GeminiCachedContentBinding)
	return binding
}

// KeyFor returns the credential holding the cache when channel is the bound
// channel and that credential is still enabled and unchanged.
func (b *GeminiCachedContentBinding) KeyFor(channel *model.Channel) (string, bool) {
	if b == nil || b.Record == nil || channel == nil || channel.Id != b.Record.ChannelId {
		return "", false
	}
	key, ok := channel.GetEnabledKeyAt(b.Record.KeyIndex)
	if !ok || model.GeminiCacheCredentialFingerprint(channel.Type, key) != b.Record.KeyFingerprint {
		return "", false
	}
	return key, true
}
