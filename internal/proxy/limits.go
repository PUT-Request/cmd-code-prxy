package proxy

import (
	"fmt"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// Default context and output limits. All models are treated as non-vision
// with a 256k context unless reconfigured; requests beyond either bound
// are rejected before any upstream call.
const (
	DefaultInputTokenLimit  = 256000
	DefaultOutputTokenLimit = 32000
)

// imageTokenEstimate approximates the vision-token cost of one image.
// Base64 payloads are stripped from the text estimate (they are not
// transmitted as text tokens), so images are billed as a flat estimate.
const imageTokenEstimate = 1500

// approxCharsPerToken is the standard English-text heuristic used by the
// dashboard estimator too; good enough for a rejection guardrail.
const approxCharsPerToken = 4

// EstimateTokens approximates the input token count of a request: JSON body
// minus base64 image payloads, scaled by chars-per-token, plus a flat
// per-image vision estimate.
func EstimateTokens(body []byte, msgs []api.OpenAIMessage) int {
	text := dataURLRe.ReplaceAll(body, nil)
	tokens := len(text) / approxCharsPerToken
	tokens += countImageURLs(msgs) * imageTokenEstimate
	return tokens
}

// checkLimits validates a request against the configured bounds.
// maxTokens is the effective output budget the client asked for
// (0 when the client did not specify one).
func (p *Proxy) checkLimits(body []byte, openAIReq api.OpenAIChatRequest) error {
	if p.limitInput > 0 {
		est := EstimateTokens(body, openAIReq.Messages)
		if est > p.limitInput {
			return fmt.Errorf("input too large: estimated ~%d tokens exceeds the %d-token context limit", est, p.limitInput)
		}
	}
	if p.limitOutput > 0 {
		requested := 0
		if openAIReq.MaxTokens != nil {
			requested = *openAIReq.MaxTokens
		}
		if openAIReq.MaxCompletionTokens != nil {
			requested = *openAIReq.MaxCompletionTokens
		}
		if requested > p.limitOutput {
			return fmt.Errorf("max_tokens %d exceeds the %d-token output limit", requested, p.limitOutput)
		}
	}
	return nil
}

// contextLimit returns the advertised context bound (0 = unlimited).
func (p *Proxy) contextLimit() int { return p.limitInput }

// effectiveMaxTokens returns the output budget to send upstream: the
// client's value when set, otherwise the configured limit (the previous
// hardcoded 64000 default would violate the output bound).
func (p *Proxy) effectiveMaxTokens(openAIReq api.OpenAIChatRequest) int {
	requested := 0
	if openAIReq.MaxTokens != nil {
		requested = *openAIReq.MaxTokens
	}
	if openAIReq.MaxCompletionTokens != nil {
		requested = *openAIReq.MaxCompletionTokens
	}
	if requested > 0 {
		return requested
	}
	if p.limitOutput > 0 {
		return p.limitOutput
	}
	return 64000
}
