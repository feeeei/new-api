package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

const (
	GeminiCachedContentActionCreate = "create"
	GeminiCachedContentActionExtend = "extend"
)

// GeminiCachedContentConsumption is one settled explicit cache charge.
type GeminiCachedContentConsumption struct {
	// Action is GeminiCachedContentActionCreate or GeminiCachedContentActionExtend.
	Action   string
	PublicID string
	Tokens   int64
	// Seconds is the storage time charged: the TTL on create, the distance the
	// expiry moved on extend, zero when storage was cancelled.
	Seconds int64
	// StorageCancelled marks a creation whose storage exceeded the available
	// quota; the cache was deleted and only its input tokens were charged.
	StorageCancelled bool
	Quota            int
	Result           *billingexpr.TieredResult
}

// RecordGeminiCachedContentConsumption adds a settled explicit cache charge
// to the user's and channel's usage and writes its consume log. A creation
// counts as a request; an extension only adds usage.
func RecordGeminiCachedContentConsumption(c *gin.Context, info *relaycommon.RelayInfo, consumption GeminiCachedContentConsumption) {
	var content *common.Message
	switch {
	case consumption.StorageCancelled:
		content = common.NewMessage("Explicit cache creation was cancelled because the storage charge exceeds the available quota; only the cached input tokens were charged")
	case consumption.Action == GeminiCachedContentActionCreate:
		content = common.NewMessage("Explicit cache created: {{tokens}} tokens stored for {{seconds}} seconds", map[string]any{"tokens": consumption.Tokens, "seconds": consumption.Seconds})
	default:
		content = common.NewMessage("Explicit cache extended: {{tokens}} tokens stored for {{seconds}} more seconds", map[string]any{"tokens": consumption.Tokens, "seconds": consumption.Seconds})
	}
	// A creation reads the cached tokens as input; an extension only stores them.
	promptTokens := 0
	if consumption.Action == GeminiCachedContentActionCreate {
		promptTokens = int(consumption.Tokens)
		model.UpdateUserUsedQuotaAndRequestCount(info.UserId, consumption.Quota)
	} else {
		model.UpdateUserUsedQuota(info.UserId, consumption.Quota)
	}
	model.UpdateChannelUsedQuota(info.ChannelId, consumption.Quota)

	explicitCache := map[string]any{
		"action":              consumption.Action,
		"name":                GeminiCachedContentNamePrefix + consumption.PublicID,
		"tokens":              consumption.Tokens,
		"seconds":             consumption.Seconds,
		"storage_token_hours": GeminiCachedContentStorageTokenHours(consumption.Tokens, time.Duration(consumption.Seconds)*time.Second),
	}
	if consumption.StorageCancelled {
		explicitCache["storage_cancelled"] = true
	}
	other := model.NewLogOther()
	other.SetPublic("explicit_cache", explicitCache)
	other.SetPublic("group_ratio", info.PriceData.GroupRatioInfo.GroupRatio)
	other.SetPublic("user_group_ratio", info.PriceData.GroupRatioInfo.GroupSpecialRatio)
	InjectTieredBillingInfo(other, info, consumption.Result)
	appendRequestPath(c, info, other)
	appendBillingInfo(info, other)
	AppendRelayLogAdminInfo(c, info, other)
	attachQuotaSaturation(c, info, other)

	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId:      info.ChannelId,
		PromptTokens:   promptTokens,
		ModelName:      info.OriginModelName,
		TokenName:      c.GetString("token_name"),
		Quota:          consumption.Quota,
		Content:        []*common.Message{content},
		TokenId:        info.TokenId,
		UseTimeSeconds: int(time.Since(info.StartTime).Seconds()),
		Group:          info.UsingGroup,
		Other:          other,
	})
}
