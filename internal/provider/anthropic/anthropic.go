// Package anthropic adapts the Anthropic Messages API to provider.Provider.
//
// Everything here was written against the anthropic-sdk-go source and the
// claude-api reference, not from memory, because both move quickly
// (DESIGN.md §8). The details that a recollection gets wrong:
//
//   - Model ids carry no date suffix: "claude-opus-5", never
//     "claude-opus-5-20260401". They are passed as plain strings; the SDK's
//     typed Model constants lag model launches.
//   - Thinking is adaptive: thinking: {type: "adaptive"}. budget_tokens is
//     rejected outright on Opus 5.
//   - Depth is output_config.effort, not a top-level effort field.
//   - Structured output is output_config.format, not the deprecated
//     output_format.
//   - Every request streams. A large max_tokens on a non-streaming request
//     hits the SDK's HTTP timeout.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/RazerBlade6/apex/internal/provider"
)

// Name is the value that appears as `provider = "anthropic"` in config.toml.
const Name = "anthropic"

// Provider is the Anthropic adapter.
type Provider struct {
	client sdk.Client
	cfg    provider.Config
}

// New builds an adapter from a resolved credential and the defaults of one
// [models.*] entry.
//
// The key and the base URL are both injected: the key so that resolution
// stays in internal/config (DESIGN.md §13), the base URL so tests can point
// the adapter at an httptest server instead of the real API.
func New(cfg provider.Config) (*Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("anthropic: no API key was provided")
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*cfg.MaxRetries))
	}
	return &Provider{client: sdk.NewClient(opts...), cfg: cfg}, nil
}

// Name identifies the vendor.
func (p *Provider) Name() string { return Name }

// capabilities records what a model accepts. Sending adaptive thinking or an
// effort level to a model that rejects it is a 400, so the digest slot being
// downgraded to claude-haiku-4-5 — which DESIGN.md §8 names as the intended
// downgrade — must not break.
type capabilities struct {
	adaptiveThinking bool
	effort           bool
}

// legacyModels are the prefixes that predate adaptive thinking. The list is a
// deny-list rather than an allow-list on purpose: a model released after this
// code was written is far more likely to take adaptive thinking than not, and
// silently dropping thinking from a new Opus would be the worse failure.
var legacyModels = []struct {
	prefix string
	caps   capabilities
}{
	// Haiku 4.5 takes neither adaptive thinking nor effort.
	{"claude-haiku-", capabilities{}},
	// Sonnet 4.5 and earlier: budget_tokens thinking, no effort.
	{"claude-sonnet-4-5", capabilities{}},
	{"claude-sonnet-4-0", capabilities{}},
	{"claude-sonnet-3", capabilities{}},
	// Opus 4.5 takes effort (low|medium|high) but not adaptive thinking.
	{"claude-opus-4-5", capabilities{effort: true}},
	{"claude-opus-4-1", capabilities{}},
	{"claude-opus-4-0", capabilities{}},
	{"claude-3", capabilities{}},
}

func capabilitiesFor(model string) capabilities {
	for _, m := range legacyModels {
		if strings.HasPrefix(model, m.prefix) {
			return m.caps
		}
	}
	return capabilities{adaptiveThinking: true, effort: true}
}

// params renders a provider.Request as an SDK request.
func (p *Provider) params(req provider.Request) (sdk.MessageNewParams, error) {
	req = p.cfg.Apply(req).WithDefaults()
	if err := req.Validate(); err != nil {
		return sdk.MessageNewParams{}, err
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
	}

	if system := strings.TrimSpace(req.System); system != "" {
		block := sdk.TextBlockParam{Text: system}
		if req.CacheSystem {
			// The cache breakpoint sits at the end of the system prompt,
			// which Prompt.Build fills with identity context and digests and
			// nothing volatile (DESIGN.md §8).
			block.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		params.System = []sdk.TextBlockParam{block}
	}

	for _, m := range req.Messages {
		block := sdk.NewTextBlock(m.Content)
		switch m.Role {
		case provider.RoleAssistant:
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(block))
		default:
			params.Messages = append(params.Messages, sdk.NewUserMessage(block))
		}
	}

	caps := capabilitiesFor(req.Model)
	if caps.adaptiveThinking {
		adaptive := sdk.ThinkingConfigAdaptiveParam{}
		params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}
	if caps.effort && req.Effort != "" {
		params.OutputConfig.Effort = sdk.OutputConfigEffort(req.Effort)
	}
	return params, nil
}

// Stream sends a request and returns a channel closed exactly once, when the
// response completes or fails.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}
	ch := make(chan provider.Event, 8)
	go p.run(ctx, params, ch)
	return ch, nil
}

// run drains one SSE stream onto ch. It owns the channel: the deferred close
// is the only one, so the channel is closed exactly once whichever way this
// returns.
func (p *Provider) run(ctx context.Context, params sdk.MessageNewParams, ch chan<- provider.Event) {
	defer close(ch)

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close() //nolint:errcheck // nothing useful to do with a close failure here

	var msg sdk.Message
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			provider.EmitFinal(ctx, ch, provider.Event{
				Type: provider.EventError,
				Err: &provider.Error{
					Provider: Name, Op: "stream", Kind: provider.KindUnknown,
					Message: provider.Truncate(err.Error()), Err: err,
				},
			})
			return
		}
		delta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent)
		if !ok {
			continue
		}
		text, ok := delta.Delta.AsAny().(sdk.TextDelta)
		if !ok || text.Text == "" {
			continue
		}
		if !provider.Emit(ctx, ch, provider.Event{Type: provider.EventTextDelta, Text: text.Text}) {
			return // ctx is done; the caller is gone
		}
	}

	if err := stream.Err(); err != nil {
		provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventError, Err: classify("stream", err)})
		return
	}
	usage := usageOf(msg)
	provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventDone, Usage: &usage})
}

// Structured constrains the response to a JSON schema and unmarshals it.
//
// It streams for the same reason Stream does: a structured call that fills a
// large max_tokens would otherwise race the HTTP timeout.
func (p *Provider) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) (provider.Usage, error) {
	if out == nil {
		return provider.Usage{}, fmt.Errorf("anthropic: Structured needs a destination to unmarshal into")
	}
	params, err := p.params(req)
	if err != nil {
		return provider.Usage{}, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(schema, &decoded); err != nil {
		return provider.Usage{}, fmt.Errorf("anthropic: output schema is not a JSON object: %w", err)
	}
	// output_config.format, not the deprecated top-level output_format.
	params.OutputConfig.Format = sdk.JSONOutputFormatParam{Schema: decoded}

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close() //nolint:errcheck // nothing useful to do with a close failure here

	// msg accumulates the whole response, so its usage block is available
	// even on a path that then fails to decode: a call that was billed
	// reports what it spent (DESIGN.md §18).
	var msg sdk.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return usageOf(msg), &provider.Error{
				Provider: Name, Op: "structured", Kind: provider.KindUnknown,
				Message: provider.Truncate(err.Error()), Err: err,
			}
		}
	}
	if err := stream.Err(); err != nil {
		return usageOf(msg), classify("structured", err)
	}
	usage := usageOf(msg)

	body := firstText(msg)
	if strings.TrimSpace(body) == "" {
		return usage, &provider.Error{
			Provider: Name, Op: "structured", Kind: provider.KindUnknown,
			Message: "the response carried no text block",
		}
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return usage, fmt.Errorf("anthropic: decode structured response: %w", err)
	}
	return usage, nil
}

// firstText returns the first text block of a message, skipping any thinking
// blocks that precede it.
func firstText(msg sdk.Message) string {
	for _, block := range msg.Content {
		if text, ok := block.AsAny().(sdk.TextBlock); ok {
			return text.Text
		}
	}
	return ""
}

// usageOf reads the accumulated message's accounting.
//
// cache_creation_input_tokens is the premium-billed number DESIGN.md §8 calls
// the one that catches an unstable cache prefix, so it is carried rather than
// dropped. The serving model comes from the message itself, not from the
// request: an alias or a fallback would otherwise be recorded as whatever was
// asked for.
func usageOf(msg sdk.Message) provider.Usage {
	u := msg.Usage
	return provider.Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadInputTokens),
		CacheWriteTokens: int(u.CacheCreationInputTokens),
		Model:            string(msg.Model),
	}
}

// classify turns an SDK error into a classified provider.Error.
//
// The SDK returns one *anthropic.Error for every non-2xx response, so the
// distinction between retryable and not comes from the status code
// (DESIGN.md §8). Anything that never reached a response — a cancelled
// context, a refused connection — carries no status and is classified from
// the transport error instead.
func classify(op string, err error) *provider.Error {
	out := &provider.Error{Provider: Name, Op: op, Err: err}

	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		out.StatusCode = apiErr.StatusCode
		out.Kind = kindOf(apiErr)
		// The vendor's own error body. Never a header, so never the key.
		if body := apiErr.RawJSON(); body != "" {
			out.Message = provider.Truncate(body)
		} else {
			out.Message = provider.Truncate(apiErr.Error())
		}
		return out
	}

	out.Kind = provider.TransportKind(err)
	out.Message = provider.Truncate(err.Error())
	return out
}

// kindOf classifies an SDK error by status code, falling back to the error
// type the body carries.
//
// The fallback is not decoration. An `error` event arriving mid-stream is
// reported against the HTTP status of the stream itself, which is 200 — the
// request was accepted and only then failed. Classifying that by status alone
// would file an overloaded_error as unclassifiable, when it is precisely the
// retryable case worth knowing about.
func kindOf(apiErr *sdk.Error) provider.Kind {
	if apiErr.StatusCode >= 400 {
		return provider.StatusKind(apiErr.StatusCode)
	}
	switch string(apiErr.Type()) {
	case "authentication_error", "permission_error":
		return provider.KindAuth
	case "rate_limit_error":
		return provider.KindRateLimit
	case "invalid_request_error", "not_found_error", "request_too_large":
		return provider.KindRequest
	case "overloaded_error", "api_error", "timeout_error":
		return provider.KindServer
	default:
		return provider.StatusKind(apiErr.StatusCode)
	}
}
