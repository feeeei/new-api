package helper

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/gin-gonic/gin"
)

// ErrGeminiCachedContentStorageNotPriced reports that the model's billing
// cannot price explicit cache storage: it is not a tiered expression, the
// expression does not reference cs, or the selected leaf is a fixed request
// price.
var ErrGeminiCachedContentStorageNotPriced = errors.New("explicit context cache storage is not priced")

// GeminiCachedContentCharge is one priced explicit cache create or extend.
type GeminiCachedContentCharge struct {
	BillingModel string
	Params       billingexpr.TokenParams
	GroupRatio   float64
	Quota        int
	Result       *billingexpr.TieredResult
}

// QuoteGeminiCachedContentCharge prices an explicit cache charge with the
// model's billing expression: inputTokens as p, contextTokens as len and
// storageTokenHours as cs. It resolves the billing model and group ratio like
// ModelPriceHelper and records the evaluation on info for the consume log.
// strict (a reservation estimate) rejects an unrepresentable quota; otherwise
// the quota saturates and the clamp is noted for auditing.
func QuoteGeminiCachedContentCharge(c *gin.Context, info *relaycommon.RelayInfo, contextTokens, inputTokens int64, storageTokenHours float64, strict bool) (*GeminiCachedContentCharge, error) {
	if matched := resolveBillingModelName(info.GetOriginModelName()); matched != "" && matched != info.OriginModelName {
		info.BillingModelName = matched
	}
	billingModel := info.GetBillingModelName()
	groupRatioInfo := HandleGroupRatio(c, info)
	info.PriceData.GroupRatioInfo = groupRatioInfo

	if billing_setting.GetBillingMode(billingModel) != billing_setting.BillingModeTieredExpr {
		return nil, ErrGeminiCachedContentStorageNotPriced
	}
	exprStr, ok := billing_setting.GetBillingExpr(billingModel)
	if !ok || strings.TrimSpace(exprStr) == "" {
		return nil, ErrGeminiCachedContentStorageNotPriced
	}
	exprHash := billingexpr.ExprHashString(exprStr)
	if !billingexpr.UsedVarsByHash(exprStr, exprHash)["cs"] {
		return nil, ErrGeminiCachedContentStorageNotPriced
	}

	// Storage charges have no request body or headers; time-based request
	// rules still apply.
	params := billingexpr.TokenParams{P: float64(inputTokens), Len: float64(contextTokens), CS: storageTokenHours}
	cost, trace, err := billingexpr.RunExprByHashWithRequest(exprStr, exprHash, params, billingexpr.RequestInput{})
	if err != nil {
		return nil, fmt.Errorf("model %s tiered expr run failed: %w", billingModel, err)
	}
	if trace.BillingUnit != billingexpr.BillingUnitToken {
		return nil, ErrGeminiCachedContentStorageNotPriced
	}
	if math.IsNaN(cost) || cost < 0 {
		return nil, fmt.Errorf("model %s tiered expr returned an invalid cost %v", billingModel, cost)
	}

	quotaBeforeGroup := cost / 1_000_000 * common.QuotaPerUnit
	var quota int
	var clamp *common.QuotaClamp
	if strict {
		quota, err = common.QuotaRoundStrict(quotaBeforeGroup * groupRatioInfo.GroupRatio)
		if err != nil {
			return nil, err
		}
	} else {
		quota, clamp = common.QuotaRoundChecked(quotaBeforeGroup * groupRatioInfo.GroupRatio)
		if clamp != nil && info.QuotaClamp == nil {
			info.QuotaClamp = clamp
		}
	}

	info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{
		BillingMode:               billing_setting.BillingModeTieredExpr,
		ModelName:                 billingModel,
		ExprString:                exprStr,
		ExprHash:                  exprHash,
		GroupRatio:                groupRatioInfo.GroupRatio,
		EstimatedPromptTokens:     int(inputTokens),
		EstimatedQuotaBeforeGroup: quotaBeforeGroup,
		EstimatedQuotaAfterGroup:  quota,
		EstimatedTier:             trace.MatchedTier,
		EstimatedBillingUnit:      trace.BillingUnit,
		QuotaPerUnit:              common.QuotaPerUnit,
		ExprVersion:               billingexpr.ExprVersion(exprStr),
	}
	return &GeminiCachedContentCharge{
		BillingModel: billingModel,
		Params:       params,
		GroupRatio:   groupRatioInfo.GroupRatio,
		Quota:        quota,
		Result: &billingexpr.TieredResult{
			BillingTokens:          &params,
			BillingUnit:            trace.BillingUnit,
			ActualQuotaBeforeGroup: quotaBeforeGroup,
			ActualQuotaAfterGroup:  quota,
			MatchedTier:            trace.MatchedTier,
			RequestRules:           trace.RequestRules,
			Clamp:                  clamp,
		},
	}, nil
}
