package billing_setting

// Built-in token prices use actual USD per million tokens. Keep new model
// defaults here instead of splitting them across the legacy ratio tables.
var builtinBillingExpr = map[string]string{
	// https://developers.openai.com/api/docs/pricing (Standard, 2026-09-09).
	// The Images API reports image output in output_tokens, normalized to c.
	"gpt-image-2":            `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-sunburst": `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-flare":    `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	// https://developers.openai.com/api/docs/models/gpt-6-astra
	// Standard pricing; the long-context rates apply to the whole request.
	// Do not infer service-tier discounts from incoming request parameters:
	// channels filter service_tier by default, so it may not reach the upstream.
	"gpt-6-astra": `len <= 272000 ? tier("standard", p * 10 + c * 50 + cr * 1 + cc * 12.5) : tier("long_context", p * 20 + c * 75 + cr * 2 + cc * 25)`,

	// https://ai.google.dev/gemini-api/docs/pricing (Paid tier, Standard, updated
	// 2026-10-09). cs is explicit context cache storage per 1M tokens per hour.
	// Thinking tokens are output. Where audio input has its own price it is ai;
	// cache reads are not split by modality, so cached audio uses the cr price.
	// Long-context tiers start above 200k prompt tokens and cover input, output
	// and cache reads; storage has one price.
	//
	// Gemini 3.8 Flash and 3.6 Flash are at introductory prices through
	// 2026-12-31. From 2027-01-01 they become p * 1.5, c * 7.5, cr * 0.15, cs * 1.
	"gemini-3.8-flash":                   `tier("standard", p * 0.75 + c * 3.75 + cr * 0.075 + cs * 0.5)`,
	"gemini-3.6-flash":                   `tier("standard", p * 0.75 + c * 3.75 + cr * 0.075 + cs * 0.5)`,
	"gemini-3.5-flash-lite":              `tier("standard", p * 0.3 + c * 2.5 + cr * 0.03 + cs * 1)`,
	"gemini-3.1-flash-lite":              `tier("standard", p * 0.25 + ai * 0.5 + c * 1.5 + cr * 0.025 + cs * 1)`,
	"gemini-3.1-pro-preview":             `len <= 200000 ? tier("standard", p * 2 + c * 12 + cr * 0.2 + cs * 4.5) : tier("long_context", p * 4 + c * 18 + cr * 0.4 + cs * 4.5)`,
	"gemini-3.1-pro-preview-customtools": `len <= 200000 ? tier("standard", p * 2 + c * 12 + cr * 0.2 + cs * 4.5) : tier("long_context", p * 4 + c * 18 + cr * 0.4 + cs * 4.5)`,
	"gemini-3-flash-preview":             `tier("standard", p * 0.5 + ai * 1 + c * 3 + cr * 0.05 + cs * 1)`,
	"gemini-2.5-pro":                     `len <= 200000 ? tier("standard", p * 1.25 + c * 10 + cr * 0.125 + cs * 4.5) : tier("long_context", p * 2.5 + c * 15 + cr * 0.25 + cs * 4.5)`,
	"gemini-2.5-flash":                   `tier("standard", p * 0.3 + ai * 1 + c * 2.5 + cr * 0.03 + cs * 1)`,
	"gemini-2.5-flash-lite":              `tier("standard", p * 0.1 + ai * 0.3 + c * 0.4 + cr * 0.01 + cs * 1)`,
}
