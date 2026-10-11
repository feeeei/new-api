package controller

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// geminiCachedContentCreateMaxAttempts bounds the upstream create calls of one
// request; channels skipped before calling upstream do not count.
const geminiCachedContentCreateMaxAttempts = 3

// geminiCachedContentUpdateLocks serialize updates of a cache within this
// process; caches share a lock only when their ids fall in the same stripe.
var geminiCachedContentUpdateLocks [64]sync.Mutex

// CreateGeminiCachedContent handles POST /v1beta/cachedContents. The request
// is charged the cached tokens' input price and their storage for the whole
// TTL.
func CreateGeminiCachedContent(c *gin.Context) {
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	body, ok := readGeminiCachedContentBody(c)
	if !ok {
		return
	}
	now := time.Now()
	req, err := service.ParseGeminiCachedContentCreateRequest(body, now, model_setting.GeminiExplicitCacheMaxTTL())
	if err != nil {
		service.AbortWithGoogleError(c, http.StatusBadRequest, err.Error())
		return
	}
	if common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		limit, _ := common.GetContextKeyType[map[string]bool](c, constant.ContextKeyTokenModelLimit)
		if !middleware.TokenModelLimitAllows(limit, req.Model) {
			service.AbortWithGoogleError(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenModelForbidden, map[string]any{"Model": req.Model}))
			return
		}
	}
	// Contents that generateContent could not accept are rejected the same way
	// here, so they cannot bypass the prompt checks below.
	tokenRequest, err := req.TokenCountRequest()
	if err != nil {
		service.AbortWithGoogleError(c, http.StatusBadRequest, err.Error())
		return
	}
	meta := tokenRequest.GetTokenCountMeta()
	if setting.ShouldCheckPromptSensitive() {
		if contains, words := service.CheckSensitiveText(meta.CombineText); contains {
			message := fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", "))
			logger.LogWarn(c, message)
			service.AbortWithGoogleError(c, http.StatusBadRequest, message)
			return
		}
	}
	publicID, err := service.NewGeminiCachedContentPublicID()
	if err != nil {
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCacheIDAllocationFailed))
		return
	}

	// One relay info spans every attempt, so the billing session opened by the
	// first reservation carries over to the channels tried after it.
	common.SetContextKey(c, constant.ContextKeyOriginalModel, req.Model)
	info := relaycommon.GenRelayInfoGemini(c, tokenRequest)
	info.ForcePreConsume = true
	info.ReserveRequiresAvailableQuota = true
	// Once settlement ran the charge stands, even if settling failed midway;
	// every other exit returns the reservation.
	settled := false
	defer func() {
		if !settled && info.Billing != nil {
			info.Billing.Refund(c)
		}
	}()
	estimate, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		service.AbortWithGoogleError(c, http.StatusInternalServerError, err.Error())
		return
	}
	estimatedTokens := int64(estimate)

	// The filter shares the map, so channels excluded below drop out of the
	// next selection, which lets lower priorities become eligible.
	excluded := make(map[int]struct{})
	service.GetChannelConstraints(c).AddFilter(taskdto.ChannelFilter{Kind: taskdto.FilterGeminiExplicitCache, ExcludedChannelIDs: excluded})
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	var lastUpstreamErr *relay.GeminiCachedContentUpstreamError
	upstreamUnreachable := false
	var ttlLimit time.Duration
	for attempts := 0; attempts < geminiCachedContentCreateMaxAttempts; {
		channel, _, err := service.CacheGetRandomSatisfiedChannel(&service.RetryParam{
			Ctx:         c,
			TokenGroup:  usingGroup,
			ModelName:   req.Model,
			RequestPath: c.Request.URL.Path,
			Retry:       common.GetPointer(0),
		})
		if err != nil || channel == nil {
			break
		}
		// Selection falls back to unfiltered candidates when it cannot load
		// them; never call a channel that was excluded or cannot cache.
		if _, seen := excluded[channel.Id]; seen {
			break
		}
		if !channel.SupportsGeminiExplicitCache() {
			excluded[channel.Id] = struct{}{}
			continue
		}
		ttl, allowed := req.TTLForChannel(channel.GeminiExplicitCacheMaxTTL())
		if !allowed {
			excluded[channel.Id] = struct{}{}
			ttlLimit = max(ttlLimit, channel.GeminiExplicitCacheMaxTTL())
			continue
		}
		if apiErr := middleware.SetupContextForSelectedChannel(c, channel, req.Model); apiErr != nil {
			excluded[channel.Id] = struct{}{}
			continue
		}
		info.InitChannelMeta(c)
		if err := helper.ModelMappedHelper(c, info, nil); err != nil {
			excluded[channel.Id] = struct{}{}
			continue
		}
		if err := helper.ApplyReasoningModelSuffix(c, info); err != nil {
			service.AbortWithGoogleError(c, http.StatusBadRequest, err.Error())
			return
		}
		upstreamModel, err := relay.GeminiCachedContentUpstreamModel(info)
		if err != nil {
			logger.LogWarn(c, common.LogText("channel #%d cannot create gemini cached content: %s", channel.Id, err.Error()))
			excluded[channel.Id] = struct{}{}
			continue
		}
		service.AppendUsedChannel(c, channel.Id)

		estimateCharge, err := helper.QuoteGeminiCachedContentCharge(c, info, estimatedTokens, estimatedTokens, service.GeminiCachedContentStorageTokenHours(estimatedTokens, ttl), true)
		if err != nil {
			abortWithGeminiCachedContentQuoteError(c, info, err)
			return
		}
		if apiErr := reserveGeminiCachedContentQuota(c, info, estimateCharge.Quota); apiErr != nil {
			abortWithGeminiCachedContentAPIError(c, apiErr)
			return
		}
		payload, err := req.UpstreamPayload(upstreamModel, ttl, publicID)
		if err != nil {
			service.AbortWithGoogleError(c, http.StatusInternalServerError, err.Error())
			return
		}

		attempts++
		upstream, err := relay.CreateGeminiCachedContentUpstream(c, info, payload)
		var upstreamErr *relay.GeminiCachedContentUpstreamError
		var malformed *relay.GeminiCachedContentMalformedResponseError
		switch {
		case err == nil:
			keyIndex := 0
			if info.ChannelIsMultiKey {
				keyIndex = info.ChannelMultiKeyIndex
			}
			// Storage is charged up to the expiry the gateway records, which an
			// upstream may set earlier than the requested TTL.
			expireTime := service.ClampGeminiCachedContentExpire(upstream.ExpireTime, now.Add(ttl)).Unix()
			storage := max(time.Duration(expireTime-now.Unix())*time.Second, 0)
			settled = registerGeminiCachedContent(c, info, &model.GeminiCachedContent{
				PublicId:        publicID,
				UserId:          info.UserId,
				TokenId:         info.TokenId,
				UsingGroup:      info.UsingGroup,
				ChannelId:       channel.Id,
				KeyIndex:        keyIndex,
				KeyFingerprint:  model.GeminiCacheCredentialFingerprint(channel.Type, info.ApiKey),
				UpstreamName:    upstream.Name,
				Model:           req.Model,
				UpstreamModel:   upstreamModel,
				DisplayName:     req.DisplayName,
				TotalTokenCount: upstream.TotalTokenCount,
				ExpireTime:      expireTime,
			}, storage)
			return
		case errors.As(err, &upstreamErr):
			logger.LogWarn(c, common.LogText("channel #%d failed to create gemini cached content: status %d", channel.Id, upstreamErr.StatusCode))
			if !upstreamErr.RetryableOnOtherChannel() {
				writeGeminiCachedContentUpstreamError(c, upstreamErr)
				return
			}
			lastUpstreamErr = upstreamErr
		case errors.As(err, &malformed):
			// Upstream may have created the cache; never create it twice.
			logger.LogError(c, common.LogText("channel #%d returned an unusable gemini cached content: %s", channel.Id, err.Error()))
			if malformed.Name != "" {
				deleteOrphanedGeminiCachedContent(c, info, malformed.Name)
			}
			service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamInvalidResponse))
			return
		default:
			logger.LogWarn(c, common.LogText("channel #%d failed to create gemini cached content: %s", channel.Id, err.Error()))
			upstreamUnreachable = true
		}
		excluded[channel.Id] = struct{}{}
	}

	switch {
	case lastUpstreamErr != nil:
		writeGeminiCachedContentUpstreamError(c, lastUpstreamErr)
	case upstreamUnreachable:
		service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
	case ttlLimit > 0:
		service.AbortWithGoogleError(c, http.StatusBadRequest, fmt.Sprintf("ttl exceeds the maximum allowed %ds by the upstream channels", int64(ttlLimit/time.Second)))
	default:
		service.AbortWithGoogleError(c, http.StatusServiceUnavailable, i18n.T(c, i18n.MsgGeminiCacheNoAvailableChannel, map[string]any{"Model": req.Model, "Group": usingGroup}))
	}
}

// registerGeminiCachedContent charges a cache created upstream for its actual
// tokens and storage time and records it, reporting whether billing was
// settled. Storage the user cannot afford deletes the cache upstream again and
// charges only the input tokens it consumed.
func registerGeminiCachedContent(c *gin.Context, info *relaycommon.RelayInfo, record *model.GeminiCachedContent, storage time.Duration) bool {
	tokens := record.TotalTokenCount
	charge, err := helper.QuoteGeminiCachedContentCharge(c, info, tokens, tokens, service.GeminiCachedContentStorageTokenHours(tokens, storage), false)
	if err != nil {
		deleteOrphanedGeminiCachedContent(c, info, record.UpstreamName)
		abortWithGeminiCachedContentQuoteError(c, info, err)
		return false
	}
	if apiErr := reserveGeminiCachedContentQuota(c, info, charge.Quota); apiErr != nil {
		deleteOrphanedGeminiCachedContent(c, info, record.UpstreamName)
		switch apiErr.GetErrorCode() {
		case types.ErrorCodeInsufficientUserQuota, types.ErrorCodePreConsumeTokenQuotaFailed:
		default:
			// Not a quota decision: nothing is charged.
			logger.LogError(c, common.LogText("failed to reserve gemini cached content %s: %s", record.PublicId, apiErr.Error()))
			abortWithGeminiCachedContentAPIError(c, apiErr)
			return false
		}
		// The saturation audit of the declined charge does not apply to this one.
		info.QuotaClamp = nil
		inputCharge, err := helper.QuoteGeminiCachedContentCharge(c, info, tokens, tokens, 0, false)
		if err != nil {
			logger.LogError(c, common.LogText("failed to price the input of cancelled gemini cached content %s: %s", record.PublicId, err.Error()))
			abortWithGeminiCachedContentAPIError(c, apiErr)
			return false
		}
		if err := service.SettleBilling(c, info, inputCharge.Quota); err != nil {
			logger.LogError(c, common.LogText("error settling billing: %s", err.Error()))
		}
		service.RecordGeminiCachedContentConsumption(c, info, service.GeminiCachedContentConsumption{
			Action:           service.GeminiCachedContentActionCreate,
			PublicID:         record.PublicId,
			Tokens:           tokens,
			StorageCancelled: true,
			Quota:            inputCharge.Quota,
			Result:           inputCharge.Result,
		})
		abortWithGeminiCachedContentAPIError(c, apiErr)
		return true
	}
	if err := model.CreateGeminiCachedContent(record); err != nil {
		logger.LogError(c, common.LogText("failed to save gemini cached content %s: %s", record.PublicId, err.Error()))
		deleteOrphanedGeminiCachedContent(c, info, record.UpstreamName)
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCachePersistFailed))
		return false
	}
	if err := service.SettleBilling(c, info, charge.Quota); err != nil {
		logger.LogError(c, common.LogText("error settling billing: %s", err.Error()))
	}
	service.RecordGeminiCachedContentConsumption(c, info, service.GeminiCachedContentConsumption{
		Action:   service.GeminiCachedContentActionCreate,
		PublicID: record.PublicId,
		Tokens:   tokens,
		Seconds:  int64(storage / time.Second),
		Quota:    charge.Quota,
		Result:   charge.Result,
	})
	c.JSON(http.StatusOK, service.GeminiCachedContentView(record))
	return true
}

// ListGeminiCachedContents handles GET /v1beta/cachedContents. Only the token's
// own live caches are listed; an upstream list would show other tenants'.
func ListGeminiCachedContents(c *gin.Context) {
	pageSize := service.GeminiCachedContentListDefaultPageSize
	if raw := strings.TrimSpace(c.Query("pageSize")); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 0 {
			service.AbortWithGoogleError(c, http.StatusBadRequest, "pageSize must be a non-negative integer")
			return
		}
		if size > 0 {
			pageSize = min(size, service.GeminiCachedContentListMaxPageSize)
		}
	}
	beforeID := 0
	if raw := strings.TrimSpace(c.Query("pageToken")); raw != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		id, err := strconv.Atoi(string(decoded))
		if decodeErr != nil || err != nil || id <= 0 {
			service.AbortWithGoogleError(c, http.StatusBadRequest, "Invalid pageToken")
			return
		}
		beforeID = id
	}

	response := gin.H{}
	// An empty group list would not limit the listing at all.
	groups := service.GeminiCachedContentUsableGroups(c)
	if len(groups) == 0 {
		c.JSON(http.StatusOK, response)
		return
	}
	records, err := model.ListGeminiCachedContentsForToken(common.GetContextKeyInt(c, constant.ContextKeyTokenId), groups, time.Now().Unix(), beforeID, pageSize+1)
	if err != nil {
		logger.LogError(c, common.LogText("failed to list gemini cached contents: %s", err.Error()))
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCacheLookupFailed))
		return
	}
	if len(records) > pageSize {
		records = records[:pageSize]
		response["nextPageToken"] = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(records[len(records)-1].Id)))
	}
	if len(records) > 0 {
		views := make([]map[string]any, 0, len(records))
		for _, record := range records {
			views = append(views, service.GeminiCachedContentView(record))
		}
		response["cachedContents"] = views
	}
	c.JSON(http.StatusOK, response)
}

// GetGeminiCachedContent handles GET /v1beta/cachedContents/:id from the
// gateway's record without calling upstream.
func GetGeminiCachedContent(c *gin.Context) {
	record, ok := lookupGeminiCachedContent(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, service.GeminiCachedContentView(record))
}

// UpdateGeminiCachedContent handles PATCH /v1beta/cachedContents/:id, which may
// only move the expiry. Moving it later is charged as storage for the time the
// stored expiry actually moved; moving it earlier refunds nothing.
func UpdateGeminiCachedContent(c *gin.Context) {
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	body, ok := readGeminiCachedContentBody(c)
	if !ok {
		return
	}
	record, ok := lookupGeminiCachedContent(c)
	if !ok {
		return
	}
	// Updates of one cache run one at a time in this process, so the order
	// upstream applies them is the order their expiries are stored and billed.
	lock := &geminiCachedContentUpdateLocks[record.Id%len(geminiCachedContentUpdateLocks)]
	lock.Lock()
	defer lock.Unlock()
	// An update that held the lock may have moved or removed the cache.
	if record, ok = lookupGeminiCachedContent(c); !ok {
		return
	}
	channel, info, ok := setupGeminiCachedContentChannel(c, record)
	// Extend only what generation can still use: the channel must still serve
	// the model in the cache's group and map it to the cached upstream model.
	if ok && model.IsChannelEnabledForGroupModel(record.UsingGroup, record.Model, record.ChannelId) &&
		helper.ModelMappedHelper(c, info, nil) == nil && helper.ApplyReasoningModelSuffix(c, info) == nil {
		ok = info.UpstreamModelName == path.Base(record.UpstreamModel)
	} else {
		ok = false
	}
	if !ok {
		service.AbortWithGoogleError(c, http.StatusServiceUnavailable, i18n.T(c, i18n.MsgGeminiCacheChannelUnavailable))
		return
	}
	maxTTL := model_setting.GeminiExplicitCacheMaxTTL()
	if channelMaxTTL := channel.GeminiExplicitCacheMaxTTL(); channelMaxTTL > 0 {
		maxTTL = min(maxTTL, channelMaxTTL)
	}
	now := time.Now()
	ttl, err := service.ParseGeminiCachedContentPatchRequest(body, c.Query("updateMask"), now, maxTTL)
	if err != nil {
		service.AbortWithGoogleError(c, http.StatusBadRequest, err.Error())
		return
	}

	info.ForcePreConsume = true
	info.ReserveRequiresAvailableQuota = true
	settled := false
	defer func() {
		if !settled && info.Billing != nil {
			info.Billing.Refund(c)
		}
	}()
	tokens := record.TotalTokenCount
	requestedExpire := now.Add(ttl)
	if extension := requestedExpire.Sub(time.Unix(record.ExpireTime, 0)); extension > 0 {
		estimateCharge, err := helper.QuoteGeminiCachedContentCharge(c, info, tokens, 0, service.GeminiCachedContentStorageTokenHours(tokens, extension), true)
		if err != nil {
			abortWithGeminiCachedContentQuoteError(c, info, err)
			return
		}
		if apiErr := reserveGeminiCachedContentQuota(c, info, estimateCharge.Quota); apiErr != nil {
			abortWithGeminiCachedContentAPIError(c, apiErr)
			return
		}
	}

	upstream, err := relay.PatchGeminiCachedContentUpstreamTTL(c, info, record.UpstreamName, ttl)
	if err != nil {
		var upstreamErr *relay.GeminiCachedContentUpstreamError
		var malformed *relay.GeminiCachedContentMalformedResponseError
		switch {
		case errors.As(err, &upstreamErr):
			if upstreamErr.NotFound() {
				if err := model.DeleteGeminiCachedContent(record.Id); err != nil {
					logger.LogError(c, common.LogText("failed to delete gemini cached content %s: %s", record.PublicId, err.Error()))
				}
			}
			writeGeminiCachedContentUpstreamError(c, upstreamErr)
		case errors.As(err, &malformed):
			logger.LogError(c, common.LogText("channel #%d returned an unusable gemini cached content: %s", channel.Id, err.Error()))
			service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamInvalidResponse))
		default:
			logger.LogError(c, common.LogText("channel #%d failed to update gemini cached content: %s", channel.Id, err.Error()))
			service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
		}
		return
	}

	newExpire := service.ClampGeminiCachedContentExpire(upstream.ExpireTime, requestedExpire).Unix()
	previousExpire, err := model.UpdateGeminiCachedContentExpireTime(record.Id, newExpire)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		service.AbortWithGeminiCachedContentNotFound(c)
		return
	}
	if err != nil {
		logger.LogError(c, common.LogText("failed to save gemini cached content %s: %s", record.PublicId, err.Error()))
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCachePersistFailed))
		return
	}
	// An update pays for the distance it moved the stored expiry, and never for
	// time before the expiry it was priced against: that time was paid already,
	// and the reservation covers only the extension beyond it.
	if extended := min(newExpire-previousExpire, newExpire-record.ExpireTime); extended > 0 {
		quota, result := info.FinalPreConsumedQuota, (*billingexpr.TieredResult)(nil)
		charge, err := helper.QuoteGeminiCachedContentCharge(c, info, tokens, 0, service.GeminiCachedContentStorageTokenHours(tokens, time.Duration(extended)*time.Second), false)
		if err != nil {
			// Like other settlements, a pricing failure keeps the reservation.
			logger.LogError(c, common.LogText("failed to price gemini cached content %s extension, keeping the reservation: %s", record.PublicId, err.Error()))
		} else {
			quota, result = charge.Quota, charge.Result
		}
		if err := service.SettleBilling(c, info, quota); err != nil {
			logger.LogError(c, common.LogText("error settling billing: %s", err.Error()))
		}
		settled = true
		service.RecordGeminiCachedContentConsumption(c, info, service.GeminiCachedContentConsumption{
			Action:   service.GeminiCachedContentActionExtend,
			PublicID: record.PublicId,
			Tokens:   tokens,
			Seconds:  extended,
			Quota:    quota,
			Result:   result,
		})
	}
	record.ExpireTime = newExpire
	record.UpdatedTime = now.Unix()
	c.JSON(http.StatusOK, service.GeminiCachedContentView(record))
}

// DeleteGeminiCachedContent handles DELETE /v1beta/cachedContents/:id. Storage
// already charged is not refunded.
func DeleteGeminiCachedContent(c *gin.Context) {
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	record, ok := lookupGeminiCachedContent(c)
	if !ok {
		return
	}
	// Without its channel credential the cache cannot be reached upstream; it
	// expires there on its own.
	if channel, info, ok := setupGeminiCachedContentChannel(c, record); ok {
		if err := relay.DeleteGeminiCachedContentUpstream(c, info, record.UpstreamName); err != nil {
			var upstreamErr *relay.GeminiCachedContentUpstreamError
			if !errors.As(err, &upstreamErr) {
				logger.LogError(c, common.LogText("channel #%d failed to delete gemini cached content: %s", channel.Id, err.Error()))
				service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
				return
			}
			if !upstreamErr.NotFound() {
				writeGeminiCachedContentUpstreamError(c, upstreamErr)
				return
			}
		}
	}
	if err := model.DeleteGeminiCachedContent(record.Id); err != nil {
		logger.LogError(c, common.LogText("failed to delete gemini cached content %s: %s", record.PublicId, err.Error()))
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCachePersistFailed))
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

func readGeminiCachedContentBody(c *gin.Context) ([]byte, bool) {
	storage, err := common.GetBodyStorage(c)
	var body []byte
	if err == nil {
		body, err = storage.Bytes()
	}
	if err != nil {
		status := http.StatusBadRequest
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		service.AbortWithGoogleError(c, status, err.Error())
		return nil, false
	}
	if len(body) == 0 {
		service.AbortWithGoogleError(c, http.StatusBadRequest, "Request body is empty")
		return nil, false
	}
	return body, true
}

// lookupGeminiCachedContent loads the current token's cache named in the path,
// answering the standard not-found 403 for a missing, expired or foreign one.
func lookupGeminiCachedContent(c *gin.Context) (*model.GeminiCachedContent, bool) {
	record, err := service.LookupGeminiCachedContent(c, c.Param("id"))
	if errors.Is(err, service.ErrGeminiCachedContentNotFound) {
		service.AbortWithGeminiCachedContentNotFound(c)
		return nil, false
	}
	if err != nil {
		logger.LogError(c, common.LogText("failed to load gemini cached content: %s", err.Error()))
		service.AbortWithGoogleError(c, http.StatusInternalServerError, i18n.T(c, i18n.MsgGeminiCacheLookupFailed))
		return nil, false
	}
	return record, true
}

// setupGeminiCachedContentChannel prepares the request for the channel
// credential holding record and returns the channel and its relay info, or
// false when that credential is no longer usable.
func setupGeminiCachedContentChannel(c *gin.Context, record *model.GeminiCachedContent) (*model.Channel, *relaycommon.RelayInfo, bool) {
	channel, err := model.CacheGetChannel(record.ChannelId)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled || !channel.SupportsGeminiExplicitCache() {
		return nil, nil, false
	}
	key, ok := (&service.GeminiCachedContentBinding{Record: record}).KeyFor(channel)
	if !ok {
		return nil, nil, false
	}
	if apiErr := middleware.SetupContextForSelectedChannel(c, channel, record.Model); apiErr != nil {
		return nil, nil, false
	}
	// Only the credential that created the cache can reach it.
	common.SetContextKey(c, constant.ContextKeyChannelKey, key)
	if channel.ChannelInfo.IsMultiKey {
		common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, record.KeyIndex)
	}
	// The cache is priced in the group it was created in.
	if common.GetContextKeyString(c, constant.ContextKeyUsingGroup) == "auto" {
		common.SetContextKey(c, constant.ContextKeyAutoGroup, record.UsingGroup)
	}
	info := relaycommon.GenRelayInfoGemini(c, nil)
	info.InitChannelMeta(c)
	service.AppendUsedChannel(c, channel.Id)
	return channel, info, true
}

// reserveGeminiCachedContentQuota raises the request's reservation to quota
// from available quota, opening the billing session on first use. A free
// group opens no session, like free models on relay requests.
func reserveGeminiCachedContentQuota(c *gin.Context, info *relaycommon.RelayInfo, quota int) *types.NewAPIError {
	if info.Billing == nil {
		if quota == 0 && info.PriceData.GroupRatioInfo.GroupRatio == 0 && !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume {
			return nil
		}
		return service.PreConsumeBilling(c, quota, info)
	}
	if err := info.Billing.Reserve(quota); err != nil {
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	return nil
}

// deleteOrphanedGeminiCachedContent deletes a cache created upstream that the
// gateway does not register, so it stops accruing storage.
func deleteOrphanedGeminiCachedContent(c *gin.Context, info *relaycommon.RelayInfo, upstreamName string) {
	if err := relay.DeleteGeminiCachedContentUpstream(c, info, upstreamName); err != nil {
		logger.LogError(c, common.LogText("failed to delete orphaned gemini cached content %s on channel #%d: %s", upstreamName, info.ChannelId, err.Error()))
	}
}

func abortWithGeminiCachedContentQuoteError(c *gin.Context, info *relaycommon.RelayInfo, err error) {
	if errors.Is(err, helper.ErrGeminiCachedContentStorageNotPriced) {
		service.AbortWithGoogleError(c, http.StatusBadRequest, i18n.T(c, i18n.MsgGeminiCacheStorageNotPriced, map[string]any{"Model": info.OriginModelName}))
		return
	}
	service.AbortWithGoogleError(c, http.StatusBadRequest, err.Error())
}

func abortWithGeminiCachedContentAPIError(c *gin.Context, apiErr *types.NewAPIError) {
	service.AbortWithGoogleError(c, apiErr.StatusCode, apiErr.ToOpenAIError().Message)
}

// writeGeminiCachedContentUpstreamError answers with an upstream
// cachedContents failure; a missing cache gets the standard 403.
func writeGeminiCachedContentUpstreamError(c *gin.Context, upstreamErr *relay.GeminiCachedContentUpstreamError) {
	if upstreamErr.NotFound() {
		service.AbortWithGeminiCachedContentNotFound(c)
		return
	}
	status := upstreamErr.StatusCode
	switch {
	case upstreamErr.Gateway && (status == http.StatusTooManyRequests || status >= http.StatusInternalServerError):
		// A declared gateway's rate limits and outages describe the upstream
		// credential holding the cache, so the client sees them as they are.
		if gjson.ValidBytes(upstreamErr.Body) {
			c.Data(status, "application/json", upstreamErr.Body)
			c.Abort()
			return
		}
	case upstreamErr.CredentialRejected():
		// A rejected channel credential is not the client's error.
		service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamRejected))
		return
	case status == http.StatusTooManyRequests:
		service.AbortWithGoogleError(c, http.StatusTooManyRequests, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
		return
	case status >= http.StatusInternalServerError || status < http.StatusBadRequest:
		service.AbortWithGoogleError(c, http.StatusBadGateway, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
		return
	default:
		// The request was invalid. Its message helps the client, but may name the
		// channel's mapped model or project, so it is masked first.
		if message := gjson.GetBytes(upstreamErr.Body, "error.message").String(); message != "" {
			service.AbortWithGoogleError(c, status, maskGeminiCachedContentUpstreamMessage(message))
			return
		}
	}
	logger.LogError(c, common.LogText("bad response status code %d, body: %s", status, common.LocalLogPreview(string(upstreamErr.Body))))
	service.AbortWithGoogleError(c, status, i18n.T(c, i18n.MsgGeminiCacheUpstreamFailed))
}

var geminiCachedContentProjectPattern = regexp.MustCompile(`projects/[^/\s"']+`)

// maskGeminiCachedContentUpstreamMessage hides URLs, keys and Google Cloud
// project ids in an upstream message shown to the client.
func maskGeminiCachedContentUpstreamMessage(message string) string {
	return geminiCachedContentProjectPattern.ReplaceAllString(common.MaskSensitiveInfo(message), "projects/***")
}
