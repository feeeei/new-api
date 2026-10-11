package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func GeminiHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	binding := service.GetGeminiCachedContentBinding(c)
	if binding != nil && binding.Record.ChannelId != info.ChannelId {
		// An admin token pin chose a channel that does not hold the cache.
		info.PerformanceBusinessRejection = true
		service.SetGeminiRawError(c, http.StatusForbidden, service.GeminiCachedContentNotFoundBody)
		return types.NewErrorWithStatusCode(errors.New(service.GeminiCachedContentNotFoundMessage), geminiCachedContentNotFoundCode, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}

	geminiReq, ok := info.Request.(*dto.GeminiChatRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected *dto.GeminiChatRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(geminiReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to GeminiChatRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	// Routing binds every cache reference it accepts; any other reference
	// (routing rejects them already) must never reach upstream.
	if binding == nil && (request.CachedContent != "" || slices.ContainsFunc(request.Requests, func(nested dto.GeminiChatRequest) bool {
		return nested.CachedContent != ""
	})) {
		info.PerformanceBusinessRejection = true
		service.SetGeminiRawError(c, http.StatusForbidden, service.GeminiCachedContentNotFoundBody)
		return types.NewErrorWithStatusCode(errors.New(service.GeminiCachedContentNotFoundMessage), geminiCachedContentNotFoundCode, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}

	// model mapped 模型映射
	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ApplyReasoningModelSuffix(c, info, request); err != nil {
		return newConvertRequestFailedError(c, info, err)
	}
	if binding != nil {
		// The cache holds content for the upstream model it was created with;
		// a channel whose model mapping has changed since cannot serve it.
		if info.UpstreamModelName != path.Base(binding.Record.UpstreamModel) {
			service.SetGeminiRawError(c, http.StatusServiceUnavailable, service.GoogleErrorBody(http.StatusServiceUnavailable, i18n.T(c, i18n.MsgGeminiCacheChannelUnavailable)))
			return types.NewErrorWithStatusCode(errors.New("the channel model mapping changed after the cached content was created"), geminiCachedContentChannelUnavailableCode, http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
		}
		request.CachedContent = binding.Record.UpstreamName
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}

	adaptor.Init(info)

	// Google rejects systemInstruction in a request that uses cachedContent.
	if info.ChannelSetting.SystemPrompt != "" && request.CachedContent == "" {
		if request.SystemInstructions == nil {
			request.SystemInstructions = &dto.GeminiChatContent{
				Parts: []dto.GeminiPart{
					{Text: info.ChannelSetting.SystemPrompt},
				},
			}
		} else if len(request.SystemInstructions.Parts) == 0 {
			request.SystemInstructions.Parts = []dto.GeminiPart{{Text: info.ChannelSetting.SystemPrompt}}
		} else if info.ChannelSetting.SystemPromptOverride {
			common.SetContextKey(c, constant.ContextKeySystemPromptOverride, true)
			merged := false
			for i := range request.SystemInstructions.Parts {
				if request.SystemInstructions.Parts[i].Text == "" {
					continue
				}
				request.SystemInstructions.Parts[i].Text = info.ChannelSetting.SystemPrompt + "\n" + request.SystemInstructions.Parts[i].Text
				merged = true
				break
			}
			if !merged {
				request.SystemInstructions.Parts = append([]dto.GeminiPart{{Text: info.ChannelSetting.SystemPrompt}}, request.SystemInstructions.Parts...)
			}
		}
	}

	// Clean up empty system instruction
	if request.SystemInstructions != nil {
		hasContent := false
		for _, part := range request.SystemInstructions.Parts {
			if part.Text != "" {
				hasContent = true
				break
			}
		}
		if !hasContent {
			request.SystemInstructions = nil
		}
	}

	var requestBody io.Reader
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		requestBody = common.NewReplayableBodyReader(storage)
		if binding != nil {
			rawBody, err := storage.Bytes()
			if err != nil {
				return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			rawBody, err = service.RewriteGeminiCachedContentReference(rawBody, binding.Record.UpstreamName)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
			body, closer, err := relaycommon.NewOutboundJSONBody(rawBody)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
			defer closer.Close()
			requestBody = body
		}
	} else {
		// 使用 ConvertGeminiRequest 转换请求格式
		convertedRequest, err := adaptor.ConvertGeminiRequest(c, info, request)
		if err != nil {
			return newConvertRequestFailedError(c, info, err)
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)
		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// apply param override
		if len(info.ParamOverride) > 0 {
			jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
		}

		logger.LogDebug(c, "Gemini request body: %s", jsonData)

		body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		jsonData = nil
		requestBody = body
	}

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		logger.LogError(c, common.LogText("Do gemini request failed: %s", err.Error()))
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			if binding != nil || request.CachedContent != "" {
				return geminiCachedContentUpstreamError(c, info, httpResp, binding != nil, statusCodeMappingStr)
			}
			newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	usage, openaiErr := adaptor.DoResponse(c, resp.(*http.Response), info)
	if openaiErr != nil {
		service.ResetStatusCode(openaiErr, statusCodeMappingStr)
		return openaiErr
	}

	service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
	return nil
}

const (
	// geminiCachedContentNotFoundCode marks a request whose explicit cache does
	// not exist for the channel it ran on. The cause is the request, not the
	// channel, so it is never retried and never disables the channel.
	geminiCachedContentNotFoundCode types.ErrorCode = "gemini_cached_content_not_found"
	// geminiCachedContentChannelUnavailableCode marks a bound request whose
	// channel can no longer serve the cache it references.
	geminiCachedContentChannelUnavailableCode types.ErrorCode = "channel_unavailable"
	// geminiUpstreamErrorBodyLimit bounds the upstream error body read for a
	// request that references an explicit cache.
	geminiUpstreamErrorBodyLimit = 1 << 20
)

// geminiCachedContentUpstreamError reports a failed upstream answer to a
// request that references an explicit context cache. A missing cache is the
// client's error: the client receives Google's standard 403 and the channel is
// neither retried nor disabled. On a declared gateway channel, a rate limit or
// outage answered to a bound request concerns the upstream credential holding
// the cache, so the client receives the gateway's answer and the channel stays
// enabled. Every other answer keeps the ordinary error handling.
func geminiCachedContentUpstreamError(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, bound bool, statusCodeMapping string) *types.NewAPIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, geminiUpstreamErrorBodyLimit))
	service.CloseResponseBodyGracefully(resp)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	newAPIError := service.RelayErrorHandler(c.Request.Context(), resp, false)

	if service.IsGeminiCachedContentNotFound(resp.StatusCode, string(body)) {
		info.PerformanceBusinessRejection = true
		service.SetGeminiRawError(c, http.StatusForbidden, service.GeminiCachedContentNotFoundBody)
		// Upstream text can name the upstream cache resource; error logs keep
		// the standard text and the server log keeps the upstream answer.
		logger.LogWarn(c, common.LogText("upstream reported the explicit cache missing: status %d, %s", resp.StatusCode, common.LocalLogPreview(newAPIError.Error())))
		return types.NewOpenAIError(errors.New(service.GeminiCachedContentNotFoundMessage), geminiCachedContentNotFoundCode, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}

	if bound && isGeminiExplicitCacheGateway(info) && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError) {
		// A fresh error: the gateway's own error code (for example a channel:
		// code of its own) must not mark this channel as failed.
		gatewayErr := types.NewOpenAIError(errors.New(newAPIError.Error()), types.ErrorCodeBadResponseStatusCode, resp.StatusCode, types.ErrOptionWithSkipRetry())
		// A body that is not JSON (an intermediary's error page) cannot be
		// returned as a Google error and gets the ordinary error body instead.
		if gjson.ValidBytes(body) {
			service.SetGeminiRawError(c, resp.StatusCode, body)
			return gatewayErr
		}
		service.ResetStatusCode(gatewayErr, statusCodeMapping)
		return gatewayErr
	}
	service.ResetStatusCode(newAPIError, statusCodeMapping)
	return newAPIError
}

func GeminiEmbeddingHandler(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	isBatch := strings.HasSuffix(c.Request.URL.Path, "batchEmbedContents")
	info.IsGeminiBatchEmbedding = isBatch

	var req dto.Request
	var err error
	var inputTexts []string

	if isBatch {
		batchRequest := &dto.GeminiBatchEmbeddingRequest{}
		err = common.UnmarshalBodyReusable(c, batchRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		}
		req = batchRequest
		for _, r := range batchRequest.Requests {
			for _, part := range r.Content.Parts {
				if part.Text != "" {
					inputTexts = append(inputTexts, part.Text)
				}
			}
		}
	} else {
		singleRequest := &dto.GeminiEmbeddingRequest{}
		err = common.UnmarshalBodyReusable(c, singleRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		}
		req = singleRequest
		for _, part := range singleRequest.Content.Parts {
			if part.Text != "" {
				inputTexts = append(inputTexts, part.Text)
			}
		}
	}

	err = helper.ModelMappedHelper(c, info, req)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ApplyReasoningModelSuffix(c, info, req); err != nil {
		return newConvertRequestFailedError(c, info, err)
	}

	req.SetModelName("models/" + info.UpstreamModelName)

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	var requestBody io.Reader
	jsonData, err := common.Marshal(req)
	if err != nil {
		return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	// apply param override
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return newAPIErrorFromParamOverride(err)
		}
	}
	logger.LogDebug(c, "Gemini embedding request body: %s", jsonData)
	body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	defer closer.Close()
	jsonData = nil
	requestBody = body

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		logger.LogError(c, common.LogText("Do gemini request failed: %s", err.Error()))
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if httpResp.StatusCode != http.StatusOK {
			newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	usage, openaiErr := adaptor.DoResponse(c, resp.(*http.Response), info)
	if openaiErr != nil {
		service.ResetStatusCode(openaiErr, statusCodeMappingStr)
		return openaiErr
	}

	service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
	return nil
}
