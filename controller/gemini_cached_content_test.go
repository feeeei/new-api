package controller

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	cachedContentAPIModel        = "gemini-cache-test"
	cachedContentAPINotFoundBody = `{"error":{"code":403,"message":"CachedContent not found (or permission denied)","status":"PERMISSION_DENIED"}}`
	cachedContentAPIContents     = `"contents":[{"role":"user","parts":[{"text":"cached document"}]}]`
)

var cachedContentAPIUserSequence atomic.Int64

type cachedContentUpstreamCall struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// cachedContentAPIFixture serves the cachedContents handlers behind TokenAuth
// against SQLite and one fake upstream that records every call.
type cachedContentAPIFixture struct {
	engine   *gin.Engine
	upstream *httptest.Server
	user     *model.User
	token    *model.Token
	other    *model.Token

	mu      sync.Mutex
	calls   []cachedContentUpstreamCall
	respond func(cachedContentUpstreamCall) (int, string)
}

func newCachedContentAPIFixture(t *testing.T, userQuota int) *cachedContentAPIFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousMemory, previousRedis, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath
	previousBatch, previousLogs, previousCount, previousQuotaPerUnit := common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "cached-contents.db")
	// The local estimate is reserved before the upstream call; every balance
	// asserted below is the settled charge for the upstream token count.
	common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit = false, true, true, 500000
	require.NoError(t, model.InitDB())
	require.NoError(t, model.InitLogDB())
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath = previousMaster, previousMemory, previousRedis, previousSQLite
		common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit = previousBatch, previousLogs, previousCount, previousQuotaPerUnit
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
	})
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.GeminiCachedContent{}, &model.Log{}))
	// $2 per 1M input tokens plus $4 per 1M token-hours of storage.
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"gemini-cache-test":"tiered_expr","gemini-nocs-test":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"gemini-cache-test":"tier(\"base\", p * 2 + cs * 4)","gemini-nocs-test":"tier(\"base\", p * 2)"}`,
		"group_ratio_setting.group_ratio": `{"default":1}`,
	}))

	f := &cachedContentAPIFixture{}
	f.user = &model.User{Id: 7310000 + int(cachedContentAPIUserSequence.Add(1)), Username: "cached-contents", Status: common.UserStatusEnabled, Group: "default", Quota: userQuota, AuthVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
	require.NoError(t, model.DB.Create(f.user).Error)
	f.token = &model.Token{UserId: f.user.Id, Key: "cachedcontentsowner", Name: "owner", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 50000}
	require.NoError(t, model.DB.Create(f.token).Error)
	f.other = &model.Token{UserId: f.user.Id, Key: "cachedcontentsother", Name: "other", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 50000}
	require.NoError(t, model.DB.Create(f.other).Error)

	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := cachedContentUpstreamCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		respond := f.respond
		f.mu.Unlock()
		status, response := http.StatusInternalServerError, `{"error":{"code":500,"message":"unexpected call","status":"INTERNAL"}}`
		if respond != nil {
			status, response = respond(call)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(f.upstream.Close)

	f.engine = gin.New()
	routes := f.engine.Group("/v1beta/cachedContents", middleware.TokenAuth())
	routes.POST("", CreateGeminiCachedContent)
	routes.GET("", ListGeminiCachedContents)
	routes.GET("/:id", GetGeminiCachedContent)
	routes.PATCH("/:id", UpdateGeminiCachedContent)
	routes.DELETE("/:id", DeleteGeminiCachedContent)
	return f
}

func (f *cachedContentAPIFixture) setResponder(respond func(cachedContentUpstreamCall) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *cachedContentAPIFixture) upstreamCalls() []cachedContentUpstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cachedContentUpstreamCall(nil), f.calls...)
}

// request authenticates with the Gemini key header when token is set.
func (f *cachedContentAPIFixture) request(t *testing.T, method, target string, token *model.Token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en")
	if token != nil {
		req.Header.Set("x-goog-api-key", "sk-"+token.Key)
	}
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, req)
	return recorder
}

// gatewayChannel declares the fake upstream as a gateway implementing the
// cachedContents API with the AI Studio wire protocol.
func (f *cachedContentAPIFixture) gatewayChannel(name, key string, priority int64, maxTTLSeconds int) *model.Channel {
	baseURL := f.upstream.URL
	channel := &model.Channel{Name: name, Type: constant.ChannelTypeGemini, Key: key, BaseURL: &baseURL, Priority: &priority}
	channel.SetOtherSettings(dto.ChannelOtherSettings{GeminiExplicitCacheUpstream: true, GeminiExplicitCacheUpstreamMaxTTLSeconds: maxTTLSeconds})
	return channel
}

func (f *cachedContentAPIFixture) addChannel(t *testing.T, channel *model.Channel) *model.Channel {
	t.Helper()
	channel.Status = common.ChannelStatusEnabled
	channel.Group = "default"
	channel.Models = cachedContentAPIModel + ",gemini-nocs-test"
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(model.DB))
	return channel
}

func (f *cachedContentAPIFixture) addRecord(t *testing.T, token *model.Token, channel *model.Channel, keyIndex int, key, upstreamName string, expireTime int64) *model.GeminiCachedContent {
	t.Helper()
	publicID, err := service.NewGeminiCachedContentPublicID()
	require.NoError(t, err)
	record := &model.GeminiCachedContent{
		PublicId: publicID, UserId: f.user.Id, TokenId: token.Id, UsingGroup: "default",
		ChannelId: channel.Id, KeyIndex: keyIndex, KeyFingerprint: model.GeminiCacheCredentialFingerprint(channel.Type, key),
		UpstreamName: upstreamName, Model: cachedContentAPIModel, UpstreamModel: "models/" + cachedContentAPIModel,
		TotalTokenCount: 2000, ExpireTime: expireTime,
	}
	require.NoError(t, model.CreateGeminiCachedContent(record))
	return record
}

// assertBalances checks the wallet and the token after the whole request:
// each charge leaves both reduced by exactly the settled quota.
func (f *cachedContentAPIFixture) assertBalances(t *testing.T, userQuota, usedQuota, requestCount, tokenRemain int) {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.First(&user, f.user.Id).Error)
	assert.Equal(t, userQuota, user.Quota)
	assert.Equal(t, usedQuota, user.UsedQuota)
	assert.Equal(t, requestCount, user.RequestCount)
	var token model.Token
	require.NoError(t, model.DB.First(&token, f.token.Id).Error)
	assert.Equal(t, tokenRemain, token.RemainQuota)
	assert.Equal(t, 50000-tokenRemain, token.UsedQuota)
}

func (f *cachedContentAPIFixture) consumeLogs(t *testing.T) []model.Log {
	t.Helper()
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	return logs
}

func cachedContentPublicID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	publicID, ok := service.ParseGeminiCachedContentPublicID(gjson.Get(response.Body.String(), "name").String())
	require.True(t, ok, response.Body.String())
	return publicID
}

func TestGeminiCachedContentCreateOnGatewayChannel(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.gatewayChannel("gateway", "gw-key-a\ngw-key-b", 0, 0)
	// Only the second key is enabled, so the cache must be bound to index 1.
	channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
	mapping := `{"gemini-cache-test":"gemini-upstream-model"}`
	channel.ModelMapping = &mapping
	f.addChannel(t, channel)
	// Upstream reports an expiry later than the requested TTL; the gateway
	// records and bills the requested TTL.
	upstreamExpire := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusOK, `{"name":"cachedContents/up1","model":"models/gemini-upstream-model","usageMetadata":{"totalTokenCount":2000},"expireTime":"` + upstreamExpire.Format(time.RFC3339) + `"}`
	})

	before := time.Now().Unix()
	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token,
		`{"model":"models/gemini-cache-test","displayName":"my cache","ttl":"1800s",`+cachedContentAPIContents+`,"systemInstruction":{"parts":[{"text":"be brief"}]}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	after := time.Now().Unix()
	publicID := cachedContentPublicID(t, response)
	view := gjson.Parse(response.Body.String())
	assert.Equal(t, "models/gemini-cache-test", view.Get("model").String())
	assert.Equal(t, "my cache", view.Get("displayName").String())
	assert.Equal(t, int64(2000), view.Get("usageMetadata.totalTokenCount").Int())

	calls := f.upstreamCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].Method)
	assert.Equal(t, "/v1beta/cachedContents", calls[0].Path)
	assert.Equal(t, "gw-key-b", calls[0].Header.Get("x-goog-api-key"))
	upstreamBody := gjson.ParseBytes(calls[0].Body)
	assert.Equal(t, "models/gemini-upstream-model", upstreamBody.Get("model").String())
	assert.Equal(t, "1800s", upstreamBody.Get("ttl").String())
	assert.Equal(t, "gw-"+publicID, upstreamBody.Get("displayName").String(), "the client display name stays local")
	assert.JSONEq(t, `[{"role":"user","parts":[{"text":"cached document"}]}]`, upstreamBody.Get("contents").Raw)
	assert.JSONEq(t, `{"parts":[{"text":"be brief"}]}`, upstreamBody.Get("systemInstruction").Raw)

	record, err := model.GetGeminiCachedContentForToken(f.token.Id, publicID)
	require.NoError(t, err)
	assert.Equal(t, f.user.Id, record.UserId)
	assert.Equal(t, "default", record.UsingGroup)
	assert.Equal(t, channel.Id, record.ChannelId)
	assert.Equal(t, 1, record.KeyIndex)
	assert.Equal(t, model.GeminiCacheCredentialFingerprint(constant.ChannelTypeGemini, "gw-key-b"), record.KeyFingerprint)
	assert.Equal(t, "cachedContents/up1", record.UpstreamName)
	assert.Equal(t, cachedContentAPIModel, record.Model)
	assert.Equal(t, "models/gemini-upstream-model", record.UpstreamModel)
	assert.Equal(t, "my cache", record.DisplayName)
	assert.Equal(t, int64(2000), record.TotalTokenCount)
	assert.GreaterOrEqual(t, record.ExpireTime, before+1800)
	assert.LessOrEqual(t, record.ExpireTime, after+1800)
	assert.Equal(t, time.Unix(record.ExpireTime, 0).UTC().Format(time.RFC3339Nano), view.Get("expireTime").String())

	// 2000 input tokens at $2/1M plus 2000 tokens * 0.5h at $4/1M:
	// (4000 + 4000) / 1e6 USD * 500000 = 4000 quota.
	f.assertBalances(t, 96000, 4000, 1, 46000)
	var stored model.Channel
	require.NoError(t, model.DB.First(&stored, channel.Id).Error)
	assert.Equal(t, int64(4000), stored.UsedQuota)
	logs := f.consumeLogs(t)
	require.Len(t, logs, 1)
	assert.Equal(t, cachedContentAPIModel, logs[0].ModelName)
	assert.Equal(t, 2000, logs[0].PromptTokens)
	assert.Zero(t, logs[0].CompletionTokens)
	assert.Equal(t, 4000, logs[0].Quota)
	assert.Equal(t, f.token.Id, logs[0].TokenId)
	assert.Equal(t, "owner", logs[0].TokenName)
	assert.Equal(t, channel.Id, logs[0].ChannelId)
	assert.Equal(t, "default", logs[0].Group)
	assert.Equal(t, "Explicit cache created: 2000 tokens stored for 1800 seconds", logs[0].Content)
	other := gjson.Parse(logs[0].Other)
	assert.JSONEq(t, `{"action":"create","name":"cachedContents/`+publicID+`","tokens":2000,"seconds":1800,"storage_token_hours":1000}`, other.Get("explicit_cache").Raw)
	assert.Equal(t, "tiered_expr", other.Get("billing_mode").String())
	assert.Equal(t, "base", other.Get("matched_tier").String())
	assert.Equal(t, "token", other.Get("billing_unit").String())
	assert.Equal(t, float64(2000), other.Get("billing_tokens.p").Float())
	assert.Equal(t, float64(2000), other.Get("billing_tokens.len").Float())
	assert.Equal(t, float64(1000), other.Get("billing_tokens.cs").Float())
	assert.Equal(t, float64(1), other.Get("group_ratio").Float())
	assert.Equal(t, "/v1beta/cachedContents", other.Get("request_path").String())
	assert.Equal(t, "wallet", other.Get("billing_source").String())
	assert.JSONEq(t, `["`+strconv.Itoa(channel.Id)+`"]`, other.Get("admin_info.use_channel").Raw)
	assert.Equal(t, int64(1), other.Get("admin_info.multi_key_index").Int())
}

// An upstream that keeps the cache for less than the requested TTL is billed
// only for the storage time the gateway records.
func TestGeminiCachedContentCreateChargesStorageUpToRecordedExpiry(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	upstreamExpire := time.Now().Add(1000 * time.Second).UTC().Truncate(time.Second)
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusOK, `{"name":"cachedContents/up1","usageMetadata":{"totalTokenCount":2000},"expireTime":"` + upstreamExpire.Format(time.RFC3339) + `"}`
	})

	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test","ttl":"1800s",`+cachedContentAPIContents+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	record, err := model.GetGeminiCachedContentForToken(f.token.Id, cachedContentPublicID(t, response))
	require.NoError(t, err)
	assert.Equal(t, upstreamExpire.Unix(), record.ExpireTime)

	logs := f.consumeLogs(t)
	require.Len(t, logs, 1)
	seconds := gjson.Get(logs[0].Other, "explicit_cache.seconds").Int()
	assert.InDelta(t, 1000, seconds, 2, "storage runs from the request to the recorded expiry, not for the full TTL")
	// 2000 input tokens at $2/1M plus 2000 tokens for `seconds` at $4/1M per hour.
	assert.Equal(t, common.QuotaRound(2000+4000*float64(seconds)/3600), logs[0].Quota)
}

const cachedContentVertexCredentials = `{"type":"service_account","project_id":"proj-1","private_key_id":"kid-1","private_key":"unused","client_email":"svc@proj-1.iam.gserviceaccount.com"}`

// addVertexChannel adds a Vertex service-account channel in us-central1 whose
// access token is already cached, so no token exchange leaves the test.
func (f *cachedContentAPIFixture) addVertexChannel(t *testing.T) *model.Channel {
	t.Helper()
	baseURL := f.upstream.URL
	channel := f.addChannel(t, &model.Channel{Name: "vertex", Type: constant.ChannelTypeVertexAi, Key: cachedContentVertexCredentials, BaseURL: &baseURL, Other: "us-central1"})
	accessTokenKey := fmt.Sprintf("access-token-%d", channel.Id)
	vertex.Cache.DeleteIf(func(key string) bool { return key == accessTokenKey })
	vertex.Cache.SetDefault(accessTokenKey, "vertex-test-token")
	t.Cleanup(func() { vertex.Cache.DeleteIf(func(key string) bool { return key == accessTokenKey }) })
	return channel
}

func TestGeminiCachedContentCreateOnVertexServiceAccount(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	f.addVertexChannel(t)
	credentials := cachedContentVertexCredentials
	const upstreamName = "projects/proj-1/locations/us-central1/cachedContents/987"
	f.setResponder(func(call cachedContentUpstreamCall) (int, string) {
		if call.Method == http.MethodDelete {
			return http.StatusOK, `{}`
		}
		return http.StatusOK, `{"name":"` + upstreamName + `","usageMetadata":{"totalTokenCount":2000}}`
	})

	before := time.Now().Unix()
	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test",`+cachedContentAPIContents+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	after := time.Now().Unix()
	publicID := cachedContentPublicID(t, response)
	calls := f.upstreamCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/projects/proj-1/locations/us-central1/cachedContents", calls[0].Path)
	assert.Equal(t, "Bearer vertex-test-token", calls[0].Header.Get("Authorization"))
	assert.Equal(t, "proj-1", calls[0].Header.Get("x-goog-user-project"))
	assert.Empty(t, calls[0].Header.Get("x-goog-api-key"))
	upstreamBody := gjson.ParseBytes(calls[0].Body)
	assert.Equal(t, "projects/proj-1/locations/us-central1/publishers/google/models/gemini-cache-test", upstreamBody.Get("model").String())
	assert.Equal(t, "3600s", upstreamBody.Get("ttl").String(), "the default TTL is one hour")

	record, err := model.GetGeminiCachedContentForToken(f.token.Id, publicID)
	require.NoError(t, err)
	assert.Equal(t, upstreamName, record.UpstreamName)
	assert.Equal(t, "projects/proj-1/locations/us-central1/publishers/google/models/gemini-cache-test", record.UpstreamModel)
	assert.Zero(t, record.KeyIndex)
	assert.Equal(t, model.GeminiCacheCredentialFingerprint(constant.ChannelTypeVertexAi, credentials), record.KeyFingerprint)
	// Without an upstream expiry the gateway records the requested TTL.
	assert.GreaterOrEqual(t, record.ExpireTime, before+3600)
	assert.LessOrEqual(t, record.ExpireTime, after+3600)
	// (2000 * 2 + 2000 token-hours * 4) / 1e6 USD * 500000 = 6000 quota.
	f.assertBalances(t, 94000, 6000, 1, 44000)

	// Deleting reaches the regional resource with the same credential and
	// refunds nothing.
	response = f.request(t, http.MethodDelete, "/v1beta/cachedContents/"+publicID, f.token, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{}`, response.Body.String())
	calls = f.upstreamCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, http.MethodDelete, calls[1].Method)
	assert.Equal(t, "/v1/"+upstreamName, calls[1].Path)
	assert.Equal(t, "Bearer vertex-test-token", calls[1].Header.Get("Authorization"))
	_, err = model.GetGeminiCachedContentForToken(f.token.Id, publicID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	f.assertBalances(t, 94000, 6000, 1, 44000)
}

func TestGeminiCachedContentCreateRejectsUnpricedStorage(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))

	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"models/gemini-nocs-test",`+cachedContentAPIContents+`}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.JSONEq(t, `{"error":{"code":400,"message":"Explicit context caching is not available for model gemini-nocs-test: storage pricing is not configured","status":"INVALID_ARGUMENT"}}`, response.Body.String())
	assert.Empty(t, f.upstreamCalls())
	f.assertBalances(t, 100000, 0, 0, 50000)
	assert.Empty(t, f.consumeLogs(t))

	response = f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, "")
	assert.JSONEq(t, `{"error":{"code":400,"message":"Request body is empty","status":"INVALID_ARGUMENT"}}`, response.Body.String())
}

// An invalid request is the client's error, but upstream's explanation may
// name the channel's project, which the client never sees.
func TestGeminiCachedContentCreateMasksUpstreamRequestErrors(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	f.addVertexChannel(t)
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusBadRequest, `{"error":{"code":400,"message":"Cached content is too small for projects/proj-1/locations/us-central1/publishers/google/models/gemini-cache-test. total_token_count=10, min_total_token_count=1024","status":"INVALID_ARGUMENT"}}`
	})

	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test",`+cachedContentAPIContents+`}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	message := gjson.Get(response.Body.String(), "error.message").String()
	assert.Contains(t, message, "projects/***")
	assert.Contains(t, message, "min_total_token_count=1024")
	assert.NotContains(t, message, "proj-1")
	assert.Len(t, f.upstreamCalls(), 1, "an invalid request is not retried on another channel")
	assert.Empty(t, f.consumeLogs(t))
}

func TestGeminiCachedContentCreateCancelsUnaffordableStorage(t *testing.T) {
	// The wallet covers the cached input (2000) but not input plus storage (4000).
	f := newCachedContentAPIFixture(t, 3000)
	f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	f.setResponder(func(call cachedContentUpstreamCall) (int, string) {
		if call.Method == http.MethodDelete {
			return http.StatusOK, `{}`
		}
		return http.StatusOK, `{"name":"cachedContents/up1","usageMetadata":{"totalTokenCount":2000}}`
	})

	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test","ttl":"1800s",`+cachedContentAPIContents+`}`)
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Equal(t, "PERMISSION_DENIED", gjson.Get(response.Body.String(), "error.status").String())
	calls := f.upstreamCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, http.MethodDelete, calls[1].Method, "the unpaid cache is deleted upstream")
	assert.Equal(t, "/v1beta/cachedContents/up1", calls[1].Path)
	assert.Equal(t, "gw-key", calls[1].Header.Get("x-goog-api-key"))
	var records int64
	require.NoError(t, model.DB.Model(&model.GeminiCachedContent{}).Count(&records).Error)
	assert.Zero(t, records)

	f.assertBalances(t, 1000, 2000, 1, 48000)
	logs := f.consumeLogs(t)
	require.Len(t, logs, 1)
	assert.Equal(t, 2000, logs[0].Quota)
	assert.Equal(t, 2000, logs[0].PromptTokens)
	assert.Equal(t, "Explicit cache creation was cancelled because the storage charge exceeds the available quota; only the cached input tokens were charged", logs[0].Content)
	other := gjson.Parse(logs[0].Other)
	assert.True(t, other.Get("explicit_cache.storage_cancelled").Bool())
	assert.Equal(t, "create", other.Get("explicit_cache.action").String())
	assert.Zero(t, other.Get("explicit_cache.seconds").Int())
	assert.Zero(t, other.Get("explicit_cache.storage_token_hours").Float())
	assert.Equal(t, float64(2000), other.Get("billing_tokens.p").Float())
	assert.False(t, other.Get("billing_tokens.cs").Exists())
}

func TestGeminiCachedContentCreateTriesAnotherChannelAfterChannelFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusTooManyRequests, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`},
		{"invalid channel key", http.StatusBadRequest, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCachedContentAPIFixture(t, 100000)
			failing := f.addChannel(t, f.gatewayChannel("failing", "gw-failing", 10, 0))
			fallback := f.addChannel(t, f.gatewayChannel("fallback", "gw-fallback", 5, 0))
			f.setResponder(func(call cachedContentUpstreamCall) (int, string) {
				if call.Header.Get("x-goog-api-key") == "gw-failing" {
					return tc.status, tc.body
				}
				return http.StatusOK, `{"name":"cachedContents/up2","usageMetadata":{"totalTokenCount":2000}}`
			})

			response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test","ttl":"1800s",`+cachedContentAPIContents+`}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			calls := f.upstreamCalls()
			require.Len(t, calls, 2)
			assert.Equal(t, "gw-failing", calls[0].Header.Get("x-goog-api-key"))
			assert.Equal(t, "gw-fallback", calls[1].Header.Get("x-goog-api-key"))
			record, err := model.GetGeminiCachedContentForToken(f.token.Id, cachedContentPublicID(t, response))
			require.NoError(t, err)
			assert.Equal(t, fallback.Id, record.ChannelId)
			f.assertBalances(t, 96000, 4000, 1, 46000)
			logs := f.consumeLogs(t)
			require.Len(t, logs, 1)
			assert.JSONEq(t, fmt.Sprintf(`["%d","%d"]`, failing.Id, fallback.Id), gjson.Get(logs[0].Other, "admin_info.use_channel").Raw)
		})
	}
}

func TestGeminiCachedContentCreateHonorsGatewayTTLLimit(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	f.addChannel(t, f.gatewayChannel("short", "gw-short", 10, 600))
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusOK, `{"name":"cachedContents/up3","usageMetadata":{"totalTokenCount":3600}}`
	})

	// An explicit TTL above the only channel's limit never reaches upstream.
	response := f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test","ttl":"3600s",`+cachedContentAPIContents+`}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.JSONEq(t, `{"error":{"code":400,"message":"ttl exceeds the maximum allowed 600s by the upstream channels","status":"INVALID_ARGUMENT"}}`, response.Body.String())
	assert.Empty(t, f.upstreamCalls())

	// Without a TTL the cache is created and charged for the channel limit:
	// (3600 * 2 + 3600 tokens * 1/6h * 4) / 1e6 USD * 500000 = 4800 quota.
	response = f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test",`+cachedContentAPIContents+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	calls := f.upstreamCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "600s", gjson.GetBytes(calls[0].Body, "ttl").String())
	f.assertBalances(t, 95200, 4800, 1, 45200)

	// A channel without that limit serves the explicit TTL; the limited one is skipped.
	f.addChannel(t, f.gatewayChannel("long", "gw-long", 5, 0))
	response = f.request(t, http.MethodPost, "/v1beta/cachedContents", f.token, `{"model":"gemini-cache-test","ttl":"3600s",`+cachedContentAPIContents+`}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	calls = f.upstreamCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "gw-long", calls[1].Header.Get("x-goog-api-key"))
	assert.Equal(t, "3600s", gjson.GetBytes(calls[1].Body, "ttl").String())
}

func TestGeminiCachedContentGetAndListAreScopedToToken(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	expireTime := time.Now().Add(time.Hour).Unix()
	first := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/a", expireTime)
	second := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/b", expireTime)
	third := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/c", expireTime)
	expired := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/d", time.Now().Add(-time.Minute).Unix())

	response := f.request(t, http.MethodGet, "/v1beta/cachedContents/"+first.PublicId, f.token, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "cachedContents/"+first.PublicId, gjson.Get(response.Body.String(), "name").String())
	assert.Equal(t, "models/"+cachedContentAPIModel, gjson.Get(response.Body.String(), "model").String())
	for _, tc := range []struct {
		name   string
		id     string
		holder *model.Token
	}{
		{"another token of the same user", first.PublicId, f.other},
		{"expired", expired.PublicId, f.token},
		{"malformed id", "Not-An-Id", f.token},
	} {
		response = f.request(t, http.MethodGet, "/v1beta/cachedContents/"+tc.id, tc.holder, "")
		assert.Equal(t, http.StatusForbidden, response.Code, tc.name)
		assert.JSONEq(t, cachedContentAPINotFoundBody, response.Body.String(), tc.name)
	}

	// Pages run newest first; ?key= authenticates like x-goog-api-key.
	listNames := func(response *httptest.ResponseRecorder) []string {
		var names []string
		for _, view := range gjson.Get(response.Body.String(), "cachedContents").Array() {
			names = append(names, view.Get("name").String())
		}
		return names
	}
	response = f.request(t, http.MethodGet, "/v1beta/cachedContents?pageSize=2&key=sk-"+f.token.Key, nil, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"cachedContents/" + third.PublicId, "cachedContents/" + second.PublicId}, listNames(response))
	pageToken := gjson.Get(response.Body.String(), "nextPageToken").String()
	assert.Equal(t, base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(second.Id))), pageToken)
	response = f.request(t, http.MethodGet, "/v1beta/cachedContents?pageSize=2&pageToken="+pageToken+"&key=sk-"+f.token.Key, nil, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"cachedContents/" + first.PublicId}, listNames(response))
	assert.False(t, gjson.Get(response.Body.String(), "nextPageToken").Exists())

	response = f.request(t, http.MethodGet, "/v1beta/cachedContents", f.other, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{}`, response.Body.String())

	response = f.request(t, http.MethodGet, "/v1beta/cachedContents?pageSize=-1", f.token, "")
	assert.JSONEq(t, `{"error":{"code":400,"message":"pageSize must be a non-negative integer","status":"INVALID_ARGUMENT"}}`, response.Body.String())
	response = f.request(t, http.MethodGet, "/v1beta/cachedContents?pageToken=bm90LWFuLWlk", f.token, "")
	assert.JSONEq(t, `{"error":{"code":400,"message":"Invalid pageToken","status":"INVALID_ARGUMENT"}}`, response.Body.String())
	assert.Empty(t, f.upstreamCalls())
}

func TestGeminiCachedContentPatchChargesTheMovedExpiry(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.gatewayChannel("gateway", "gw-key-a\ngw-key-b", 0, 0)
	channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	f.addChannel(t, channel)
	// The cache was created with key index 1 while polling now selects index 0.
	storedExpire := time.Now().Add(30 * time.Minute).Unix()
	record := f.addRecord(t, f.token, channel, 1, "gw-key-b", "cachedContents/up1", storedExpire)
	var upstreamExpire time.Time
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusOK, `{"name":"cachedContents/up1","expireTime":"` + upstreamExpire.Format(time.RFC3339) + `"}`
	})
	target := "/v1beta/cachedContents/" + record.PublicId + "?updateMask=ttl"

	// Upstream moves the expiry exactly one hour past the stored one.
	upstreamExpire = time.Unix(storedExpire+3600, 0).UTC()
	response := f.request(t, http.MethodPatch, target, f.token, `{"ttl":"7200s"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, upstreamExpire.Format(time.RFC3339Nano), gjson.Get(response.Body.String(), "expireTime").String())
	calls := f.upstreamCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPatch, calls[0].Method)
	assert.Equal(t, "/v1beta/cachedContents/up1", calls[0].Path)
	assert.Equal(t, "updateMask=ttl", calls[0].Query)
	assert.JSONEq(t, `{"ttl":"7200s"}`, string(calls[0].Body))
	assert.Equal(t, "gw-key-b", calls[0].Header.Get("x-goog-api-key"), "only the creating key holds the cache")
	stored, err := model.GetGeminiCachedContentForToken(f.token.Id, record.PublicId)
	require.NoError(t, err)
	assert.Equal(t, storedExpire+3600, stored.ExpireTime)
	// 2000 tokens stored one more hour at $4/1M token-hours: 8000 / 1e6 USD * 500000.
	f.assertBalances(t, 96000, 4000, 0, 46000)
	logs := f.consumeLogs(t)
	require.Len(t, logs, 1)
	assert.Equal(t, 4000, logs[0].Quota)
	assert.Zero(t, logs[0].PromptTokens)
	assert.Equal(t, "Explicit cache extended: 2000 tokens stored for 3600 more seconds", logs[0].Content)
	other := gjson.Parse(logs[0].Other)
	assert.JSONEq(t, `{"action":"extend","name":"cachedContents/`+record.PublicId+`","tokens":2000,"seconds":3600,"storage_token_hours":2000}`, other.Get("explicit_cache").Raw)
	assert.Zero(t, other.Get("billing_tokens.p").Float())
	assert.Equal(t, float64(2000), other.Get("billing_tokens.len").Float())
	assert.Equal(t, float64(2000), other.Get("billing_tokens.cs").Float())

	// Shortening charges and refunds nothing.
	upstreamExpire = time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	response = f.request(t, http.MethodPatch, target, f.token, `{"ttl":"60s"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	stored, err = model.GetGeminiCachedContentForToken(f.token.Id, record.PublicId)
	require.NoError(t, err)
	assert.Equal(t, upstreamExpire.Unix(), stored.ExpireTime)
	f.assertBalances(t, 96000, 4000, 0, 46000)
	assert.Len(t, f.consumeLogs(t), 1)

	// Upstream no longer holds the cache: the record is forgotten and the
	// extension reserved for the request is returned.
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		return http.StatusNotFound, `{"error":{"code":404,"message":"Cached content up1 not found","status":"NOT_FOUND"}}`
	})
	response = f.request(t, http.MethodPatch, target, f.token, `{"ttl":"7200s"}`)
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.JSONEq(t, cachedContentAPINotFoundBody, response.Body.String())
	_, err = model.GetGeminiCachedContentForToken(f.token.Id, record.PublicId)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.Eventually(t, func() bool {
		var user model.User
		var token model.Token
		return model.DB.First(&user, f.user.Id).Error == nil && model.DB.First(&token, f.token.Id).Error == nil &&
			user.Quota == 96000 && token.RemainQuota == 46000
	}, 3*time.Second, 10*time.Millisecond)
	assert.Len(t, f.consumeLogs(t), 1)
}

// A shortening stored by another node while this update is upstream must not
// make the update pay again for time before the expiry it was priced against.
func TestGeminiCachedContentPatchNeverChargesBeyondItsReservation(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	record := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/up1", time.Now().Add(2*time.Hour).Unix())
	upstreamExpire := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.setResponder(func(cachedContentUpstreamCall) (int, string) {
		assert.NoError(t, model.DB.Model(&model.GeminiCachedContent{}).Where("id = ?", record.Id).
			Update("expire_time", time.Now().Add(10*time.Minute).Unix()).Error)
		return http.StatusOK, `{"name":"cachedContents/up1","expireTime":"` + upstreamExpire.Format(time.RFC3339) + `"}`
	})

	response := f.request(t, http.MethodPatch, "/v1beta/cachedContents/"+record.PublicId, f.token, `{"ttl":"3600s"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	stored, err := model.GetGeminiCachedContentForToken(f.token.Id, record.PublicId)
	require.NoError(t, err)
	assert.Equal(t, upstreamExpire.Unix(), stored.ExpireTime)
	f.assertBalances(t, 100000, 0, 0, 50000)
	assert.Empty(t, f.consumeLogs(t))
}

// An update must not keep paying for a cache that generation can no longer
// use on its channel.
func TestGeminiCachedContentPatchRefusesCacheGenerationCannotUse(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	record := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/up1", time.Now().Add(time.Hour).Unix())
	require.NoError(t, model.DB.Model(channel).Update("model_mapping", `{"gemini-cache-test":"gemini-remapped"}`).Error)

	response := f.request(t, http.MethodPatch, "/v1beta/cachedContents/"+record.PublicId, f.token, `{"ttl":"7200s"}`)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.JSONEq(t, `{"error":{"code":503,"message":"The channel holding this cached content is temporarily unavailable","status":"UNAVAILABLE"}}`, response.Body.String())
	assert.Empty(t, f.upstreamCalls())
	f.assertBalances(t, 100000, 0, 0, 50000)
}

func TestGeminiCachedContentDeleteForgetsCacheMissingUpstream(t *testing.T) {
	f := newCachedContentAPIFixture(t, 100000)
	channel := f.addChannel(t, f.gatewayChannel("gateway", "gw-key", 0, 0))
	expireTime := time.Now().Add(time.Hour).Unix()
	gone := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/gone", expireTime)
	kept := f.addRecord(t, f.token, channel, 0, "gw-key", "cachedContents/kept", expireTime)
	const rejected = `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`
	f.setResponder(func(call cachedContentUpstreamCall) (int, string) {
		if call.Path == "/v1beta/cachedContents/gone" {
			return http.StatusForbidden, cachedContentAPINotFoundBody
		}
		return http.StatusBadRequest, rejected
	})

	response := f.request(t, http.MethodDelete, "/v1beta/cachedContents/"+gone.PublicId, f.token, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{}`, response.Body.String())
	_, err := model.GetGeminiCachedContentForToken(f.token.Id, gone.PublicId)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// Any other upstream failure keeps the record so the client can retry.
	response = f.request(t, http.MethodDelete, "/v1beta/cachedContents/"+kept.PublicId, f.token, "")
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.JSONEq(t, rejected, response.Body.String())
	_, err = model.GetGeminiCachedContentForToken(f.token.Id, kept.PublicId)
	require.NoError(t, err)
	assert.Len(t, f.upstreamCalls(), 2)
}

const geminiCacheRelayModel = "gemini-explicit-cache-test"

type geminiCacheUpstreamCall struct {
	path string
	key  string
	body []byte
}

// geminiCacheRelayFixture serves /v1beta/models through the real TokenAuth,
// Distribute and Relay. The explicit cache lives on the second key of a
// declared gateway channel; a higher-priority channel serves the same model,
// so any request that is not pinned to the cache, or that retries, reaches it.
type geminiCacheRelayFixture struct {
	engine  *gin.Engine
	user    *model.User
	token   *model.Token
	gateway *model.Channel
	other   *model.Channel
	record  *model.GeminiCachedContent

	mu         sync.Mutex
	reply      func(http.ResponseWriter)
	calls      []geminiCacheUpstreamCall
	otherCalls int
}

func setupGeminiCacheRelayTest(t *testing.T) *geminiCacheRelayFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousSQLite := common.IsMasterNode, common.SQLitePath
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	previousBatch, previousConsumeLog, previousCountToken := common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken
	previousQuotaPerUnit, previousRetryTimes := common.QuotaPerUnit, common.RetryTimes
	previousAutoDisable, previousErrorLog := common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = false, false, false
	common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken = false, true, false
	common.QuotaPerUnit, common.RetryTimes = 500000, 3
	common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled = true, true
	common.SQLitePath = filepath.Join(t.TempDir(), "gemini-cache-relay.db")
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.IsMasterNode, common.SQLitePath = previousMaster, previousSQLite
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken = previousBatch, previousConsumeLog, previousCountToken
		common.QuotaPerUnit, common.RetryTimes = previousQuotaPerUnit, previousRetryTimes
		common.AutomaticDisableChannelEnabled, constant.ErrorLogEnabled = previousAutoDisable, previousErrorLog
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{},
		&model.PerfMetric{}, &model.GeminiCachedContent{}))

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		switch key {
		case "billing_setting.billing_mode", "billing_setting.billing_expr", "group_ratio_setting.group_ratio", "perf_metrics_setting.enabled":
			saved[key] = value
		}
		return nil
	}))
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(saved)) })
	// Cached tokens are priced separately from the rest of the prompt.
	expressions, err := common.Marshal(map[string]string{geminiCacheRelayModel: `tier("base", p * 2 + c * 8 + cr * 0.5)`})
	require.NoError(t, err)
	// Health samples stay off except where a test asserts on them: recording
	// one starts a goroutine that reads globals this fixture restores.
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"` + geminiCacheRelayModel + `":"tiered_expr"}`,
		"billing_setting.billing_expr":    string(expressions),
		"group_ratio_setting.group_ratio": `{"default":1}`,
		"perf_metrics_setting.enabled":    "false",
	}))

	fixture := &geminiCacheRelayFixture{}
	gatewayUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, geminiCacheUpstreamCall{path: r.URL.Path, key: r.Header.Get("x-goog-api-key"), body: body})
		reply := fixture.reply
		fixture.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(gatewayUpstream.Close)
	otherUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		fixture.otherCalls++
		fixture.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(otherUpstream.Close)

	fixture.user = &model.User{Username: "gemini-cache-user", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default",
		Quota: 1000000, AuthVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
	require.NoError(t, model.DB.Create(fixture.user).Error)
	fixture.token = &model.Token{UserId: fixture.user.Id, Name: "gemini-cache", Key: "geminicacherelaytoken", Status: common.TokenStatusEnabled,
		ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, model.DB.Create(fixture.token).Error)

	fixture.gateway = &model.Channel{Name: "gemini-cache-gateway", Type: constant.ChannelTypeGemini, Key: "gateway-key-0\ngateway-key-1",
		Status: common.ChannelStatusEnabled, Group: "default", Models: geminiCacheRelayModel, BaseURL: &gatewayUpstream.URL,
		Priority: common.GetPointer(int64(0)), AutoBan: common.GetPointer(1),
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
	fixture.gateway.SetSetting(dto.ChannelSettings{SystemPrompt: "channel system prompt"})
	fixture.gateway.SetOtherSettings(dto.ChannelOtherSettings{GeminiExplicitCacheUpstream: true})
	fixture.other = &model.Channel{Name: "gemini-cache-other", Type: constant.ChannelTypeGemini, Key: "other-key", Status: common.ChannelStatusEnabled,
		Group: "default", Models: geminiCacheRelayModel, BaseURL: &otherUpstream.URL, Priority: common.GetPointer(int64(10)), AutoBan: common.GetPointer(1)}
	for _, channel := range []*model.Channel{fixture.gateway, fixture.other} {
		require.NoError(t, model.DB.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
	}

	fixture.record = &model.GeminiCachedContent{
		PublicId: "relaycache", UserId: fixture.user.Id, TokenId: fixture.token.Id, UsingGroup: "default", ChannelId: fixture.gateway.Id, KeyIndex: 1,
		KeyFingerprint: model.GeminiCacheCredentialFingerprint(constant.ChannelTypeGemini, "gateway-key-1"),
		UpstreamName:   "cachedContents/upstream-relay-1", Model: geminiCacheRelayModel, UpstreamModel: "models/" + geminiCacheRelayModel,
		TotalTokenCount: 800, ExpireTime: time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, model.CreateGeminiCachedContent(fixture.record))

	fixture.engine = gin.New()
	fixture.engine.POST("/v1beta/models/*path", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) {
		Relay(c, types.RelayFormatGemini)
	})
	return fixture
}

// relay sends one generateContent request with apiKey while the cache gateway
// answers with reply, and returns the client response and the upstream traffic.
func (f *geminiCacheRelayFixture) relay(t *testing.T, apiKey string, reply func(http.ResponseWriter), body string) (*httptest.ResponseRecorder, []geminiCacheUpstreamCall, int) {
	t.Helper()
	f.mu.Lock()
	f.reply, f.calls, f.otherCalls = reply, nil, 0
	f.mu.Unlock()
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+geminiCacheRelayModel+":generateContent", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Language", "en")
	request.Header.Set("x-goog-api-key", apiKey)
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	f.mu.Lock()
	defer f.mu.Unlock()
	return recorder, f.calls, f.otherCalls
}

// assertChannelKeptEnabled checks that the failure neither disabled the
// gateway channel or its key nor asked for that: the request policy recorded
// in the error log mirrors the automatic-disable decision synchronously.
func (f *geminiCacheRelayFixture) assertChannelKeptEnabled(t *testing.T) {
	t.Helper()
	var errorLog model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeError).Order("id desc").First(&errorLog).Error)
	other, err := common.StrToMap(errorLog.Other)
	require.NoError(t, err)
	adminInfo, _ := other["admin_info"].(map[string]any)
	events, _ := adminInfo["request_policy"].([]any)
	var health []string
	for _, event := range events {
		if value, ok := event.(map[string]any)["health"].(string); ok {
			health = append(health, value)
		}
	}
	assert.Equal(t, []string{"unchanged"}, health)

	channel, err := model.GetChannelById(f.gateway.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
	assert.NotContains(t, channel.ChannelInfo.MultiKeyStatusList, f.record.KeyIndex)
}

// geminiCacheHealthSamples counts the model health samples of the test model;
// classification happens synchronously when the relay returns.
func geminiCacheHealthSamples(t *testing.T) int64 {
	t.Helper()
	result, err := perfmetrics.QuerySummaryAll(2, nil)
	require.NoError(t, err)
	for _, summary := range result.Models {
		if summary.ModelName == geminiCacheRelayModel {
			return summary.RequestCount
		}
	}
	return 0
}

func geminiCacheReply(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestGeminiCachedContentRelay(t *testing.T) {
	f := setupGeminiCacheRelayTest(t)
	reference := service.GeminiCachedContentNamePrefix + f.record.PublicId
	clientBody := `{"cachedContent":"` + reference + `","contents":[{"role":"user","parts":[{"text":"summarize"}]}],"generationConfig":{"temperature":0.10}}`
	answered := geminiCacheReply(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"cached answer"}]},"finishReason":"STOP"}],`+
		`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"totalTokenCount":1100,"cachedContentTokenCount":800},"modelVersion":"`+geminiCacheRelayModel+`"}`)

	t.Run("upstream receives its own resource name on the key holding the cache", func(t *testing.T) {
		recorder, calls, otherCalls := f.relay(t, f.token.Key, answered, clientBody)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "cached answer")
		require.Len(t, calls, 1)
		assert.Zero(t, otherCalls, "the higher-priority channel does not hold the cache")
		assert.Equal(t, "/v1beta/models/"+geminiCacheRelayModel+":generateContent", calls[0].path)
		assert.Equal(t, "gateway-key-1", calls[0].key)
		assert.Equal(t, f.record.UpstreamName, gjson.GetBytes(calls[0].body, "cachedContent").String())
		assert.False(t, gjson.GetBytes(calls[0].body, "systemInstruction").Exists(), "Google rejects systemInstruction together with cachedContent")

		// p = 1000 - 800, cr = 800, c = 100: (200*2 + 100*8 + 800*0.5) / 1e6 * 500000.
		var consumeLog model.Log
		require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id desc").First(&consumeLog).Error)
		assert.Equal(t, 800, consumeLog.Quota)
		assert.Equal(t, 1000, consumeLog.PromptTokens)
		other, err := common.StrToMap(consumeLog.Other)
		require.NoError(t, err)
		assert.EqualValues(t, 800, other["cache_tokens"])
	})

	t.Run("pass-through body changes only the cache reference", func(t *testing.T) {
		settings := model_setting.GetGlobalSettings()
		previous := settings.PassThroughRequestEnabled
		settings.PassThroughRequestEnabled = true
		t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })

		recorder, calls, _ := f.relay(t, f.token.Key, answered, clientBody)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Len(t, calls, 1)
		assert.Equal(t, f.record.UpstreamName, gjson.GetBytes(calls[0].body, "cachedContent").String())
		assert.Equal(t, "0.10", gjson.GetBytes(calls[0].body, "generationConfig.temperature").Raw, "the client body is forwarded verbatim")
		assert.False(t, gjson.GetBytes(calls[0].body, "systemInstruction").Exists())
	})

	t.Run("missing upstream cache returns the standard 403 without retry or disabling", func(t *testing.T) {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "true"}))
		t.Cleanup(func() {
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "false"}))
		})
		require.True(t, perf_metrics_setting.GetSetting().Enabled)
		samples := geminiCacheHealthSamples(t)
		recorder, calls, otherCalls := f.relay(t, f.token.Key, geminiCacheReply(http.StatusForbidden,
			`{"error":{"code":403,"message":"CachedContent not found (or permission denied)","status":"PERMISSION_DENIED"}}`), clientBody)
		assert.Equal(t, http.StatusForbidden, recorder.Code)
		assert.Equal(t, string(service.GeminiCachedContentNotFoundBody), recorder.Body.String())
		assert.Len(t, calls, 1)
		assert.Zero(t, otherCalls)
		f.assertChannelKeptEnabled(t)
		assert.Equal(t, samples, geminiCacheHealthSamples(t), "a missing cache is the client's error, not a health sample")
	})

	t.Run("gateway rate limit is returned verbatim and keeps the channel enabled", func(t *testing.T) {
		rateLimited := `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED"}}`
		recorder, calls, otherCalls := f.relay(t, f.token.Key, geminiCacheReply(http.StatusTooManyRequests, rateLimited), clientBody)
		assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
		assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
		assert.Equal(t, rateLimited, recorder.Body.String())
		assert.Len(t, calls, 1)
		assert.Zero(t, otherCalls)
		f.assertChannelKeptEnabled(t)
	})

	t.Run("gateway error carrying its own channel code keeps the channel enabled", func(t *testing.T) {
		failed := `{"error":{"code":"channel:param_override_invalid","message":"downstream channel misconfigured","status":"INTERNAL"}}`
		recorder, calls, otherCalls := f.relay(t, f.token.Key, geminiCacheReply(http.StatusInternalServerError, failed), clientBody)
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Equal(t, failed, recorder.Body.String())
		assert.Len(t, calls, 1)
		assert.Zero(t, otherCalls)
		f.assertChannelKeptEnabled(t)
	})

	t.Run("another spelling of the reference is rejected before reaching upstream", func(t *testing.T) {
		variant := `{"CachedContent":"` + f.record.UpstreamName + `","contents":[{"role":"user","parts":[{"text":"summarize"}]}]}`
		recorder, calls, otherCalls := f.relay(t, f.token.Key, answered, variant)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.JSONEq(t, `{"error":{"code":400,"message":"CachedContent is not supported, use cachedContent","status":"INVALID_ARGUMENT"}}`, recorder.Body.String())
		assert.Empty(t, calls)
		assert.Zero(t, otherCalls)
	})

	t.Run("admin channel pin elsewhere answers the standard 403 without an upstream call", func(t *testing.T) {
		require.NoError(t, model.DB.Model(f.user).Update("role", common.RoleAdminUser).Error)
		t.Cleanup(func() { require.NoError(t, model.DB.Model(f.user).Update("role", common.RoleCommonUser).Error) })
		recorder, calls, otherCalls := f.relay(t, f.token.Key+"-"+strconv.Itoa(f.other.Id), answered, clientBody)
		assert.Equal(t, http.StatusForbidden, recorder.Code)
		assert.Equal(t, string(service.GeminiCachedContentNotFoundBody), recorder.Body.String())
		assert.Empty(t, calls)
		assert.Zero(t, otherCalls)
	})

	t.Run("changed channel model mapping cannot serve the cache", func(t *testing.T) {
		require.NoError(t, model.DB.Model(f.gateway).Update("model_mapping", `{"`+geminiCacheRelayModel+`":"gemini-remapped"}`).Error)
		t.Cleanup(func() { require.NoError(t, model.DB.Model(f.gateway).Update("model_mapping", "").Error) })
		recorder, calls, otherCalls := f.relay(t, f.token.Key, answered, clientBody)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.JSONEq(t, `{"error":{"code":503,"message":"The channel holding this cached content is temporarily unavailable","status":"UNAVAILABLE"}}`, recorder.Body.String())
		assert.Empty(t, calls)
		assert.Zero(t, otherCalls)
	})
}

// TestGeminiCachedContentDatabaseMatrix runs the explicit cache store on every
// supported database: the startup migration (twice), ownership, soft deletion,
// listing, and the compare-and-swap expiry update that extension billing uses.
func TestGeminiCachedContentDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			modelManagementDB(t, dialect.kind, os.Getenv(dialect.env))
			// The helper starts a replica; the master node runs the full startup migration.
			common.IsMasterNode = true
			for range 2 {
				require.NoError(t, model.InitDB(), "startup migration must succeed on an empty and on a migrated database")
				connection, err := model.DB.DB()
				require.NoError(t, err)
				t.Cleanup(func() { _ = connection.Close() })
			}
			db := model.DB
			var migrationLog strings.Builder
			recorder := logger.New(log.New(&migrationLog, "", 0), logger.Config{LogLevel: logger.Info})
			require.NoError(t, db.Session(&gorm.Session{Logger: recorder}).AutoMigrate(&model.GeminiCachedContent{}))
			assert.Contains(t, migrationLog.String(), "gemini_cached_contents")
			assert.NotRegexp(t, `(?i)\b(ALTER|CREATE|DROP)\s+(TABLE|INDEX|UNIQUE)\b`, migrationLog.String(), "a restart must not alter the migrated table")

			const owner, stranger = 7, 8
			newest := &model.GeminiCachedContent{
				PublicId: strings.Repeat("z", 64), UserId: 3, TokenId: owner, UsingGroup: "default", ChannelId: 11, KeyIndex: 2,
				KeyFingerprint: strings.Repeat("ab", 32), UpstreamName: "projects/p/locations/us-central1/cachedContents/123",
				Model: strings.Repeat("m", 255), UpstreamModel: "projects/p/locations/us-central1/publishers/google/models/gemini-2.5-pro",
				DisplayName: strings.Repeat("缓🙂", 64), TotalTokenCount: 1048576, ExpireTime: 2000,
			}
			records := []*model.GeminiCachedContent{
				{PublicId: "old", TokenId: owner, UsingGroup: "default", ExpireTime: 2000},
				{PublicId: "vip", TokenId: owner, UsingGroup: "vip", ExpireTime: 2000},
				{PublicId: "expired", TokenId: owner, UsingGroup: "default", ExpireTime: 1000},
				{PublicId: "foreign", TokenId: stranger, UsingGroup: "default", ExpireTime: 2000},
				{PublicId: "deleted", TokenId: owner, UsingGroup: "default", ExpireTime: 2000},
				newest,
			}
			for _, record := range records {
				require.NoError(t, model.CreateGeminiCachedContent(record))
				assert.NotZero(t, record.CreatedTime)
			}
			require.Error(t, model.CreateGeminiCachedContent(&model.GeminiCachedContent{PublicId: "old", TokenId: stranger, ExpireTime: 2000}), "public ids are unique across tokens")
			deleted := records[4]
			require.NoError(t, model.DeleteGeminiCachedContent(deleted.Id))
			var tombstone model.GeminiCachedContent
			require.NoError(t, db.Unscoped().First(&tombstone, deleted.Id).Error)
			assert.True(t, tombstone.DeletedAt.Valid, "deletion keeps the row")

			for _, tc := range []struct {
				name     string
				tokenId  int
				publicId string
				want     *model.GeminiCachedContent
			}{
				{"owner reads every stored field back", owner, newest.PublicId, newest},
				{"another token", stranger, newest.PublicId, nil},
				{"deleted", owner, deleted.PublicId, nil},
				{"unknown", owner, "missing", nil},
			} {
				got, err := model.GetGeminiCachedContentForToken(tc.tokenId, tc.publicId)
				if tc.want == nil {
					assert.ErrorIs(t, err, gorm.ErrRecordNotFound, tc.name)
					continue
				}
				require.NoError(t, err, tc.name)
				assert.Equal(t, tc.want, got, tc.name)
			}

			for _, tc := range []struct {
				name     string
				groups   []string
				activeAt int64
				beforeId int
				limit    int
				want     []string
			}{
				{"newest first without deleted, foreign or expired", nil, 1000, 0, 10, []string{newest.PublicId, "vip", "old"}},
				{"group filter", []string{"default"}, 1000, 0, 10, []string{newest.PublicId, "old"}},
				{"first page", []string{"default", "vip"}, 1000, 0, 2, []string{newest.PublicId, "vip"}},
				{"next page", []string{"default", "vip"}, 1000, records[1].Id, 2, []string{"old"}},
				{"expiry is exclusive", nil, 999, 0, 10, []string{newest.PublicId, "expired", "vip", "old"}},
				{"no usable group", []string{"other"}, 1000, 0, 10, nil},
			} {
				page, err := model.ListGeminiCachedContentsForToken(owner, tc.groups, tc.activeAt, tc.beforeId, tc.limit)
				require.NoError(t, err, tc.name)
				var got []string
				for _, record := range page {
					got = append(got, record.PublicId)
				}
				assert.Equal(t, tc.want, got, tc.name)
			}

			// A concurrent writer stores these values right after the update reads
			// the current expiry, so its compare-and-swap misses. The callback lives
			// on this subtest's connection, which is closed at cleanup.
			target := records[0]
			var concurrentWrites []int64
			require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:concurrent_expiry_writer", func(tx *gorm.DB) {
				if len(concurrentWrites) == 0 || tx.Statement.Table != "gemini_cached_contents" {
					return
				}
				value := concurrentWrites[0]
				concurrentWrites = concurrentWrites[1:]
				require.NoError(t, db.Model(&model.GeminiCachedContent{}).Where("id = ?", target.Id).UpdateColumn("expire_time", value).Error)
			}))
			for _, tc := range []struct {
				name             string
				start, expire    int64
				concurrent       []int64
				previous, stored int64
				err              error
			}{
				{name: "extend", start: 2000, expire: 3000, previous: 2000, stored: 3000},
				{name: "same value", start: 3000, expire: 3000, previous: 3000, stored: 3000},
				{name: "the concurrent value is the predecessor", start: 2000, expire: 4000, concurrent: []int64{3000}, previous: 3000, stored: 4000},
				{name: "retries exhausted", start: 2000, expire: 9000, concurrent: []int64{2001, 2002, 2003, 2004, 2005}, err: model.ErrGeminiCachedContentConcurrentUpdate, stored: 2005},
			} {
				require.NoError(t, db.Model(&model.GeminiCachedContent{}).Where("id = ?", target.Id).UpdateColumn("expire_time", tc.start).Error)
				concurrentWrites = tc.concurrent
				previous, err := model.UpdateGeminiCachedContentExpireTime(target.Id, tc.expire)
				if tc.err != nil {
					assert.ErrorIs(t, err, tc.err, tc.name)
				} else {
					require.NoError(t, err, tc.name)
					assert.Equal(t, tc.previous, previous, tc.name)
				}
				assert.Empty(t, concurrentWrites, tc.name)
				var stored model.GeminiCachedContent
				require.NoError(t, db.First(&stored, target.Id).Error)
				assert.Equal(t, tc.stored, stored.ExpireTime, tc.name)
			}
			_, err := model.UpdateGeminiCachedContentExpireTime(deleted.Id, 5000)
			assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "a deleted cache cannot be extended")
		})
	}
}
