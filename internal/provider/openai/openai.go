// Package openai adapts the OpenAI Chat Completions API to provider.Provider.
//
// Written against the openai-go source rather than by analogy with the
// Anthropic adapter: the two SDKs look similar and are not
// (DESIGN.md §8). What differs here:
//
//   - Token usage only arrives on a stream when stream_options.include_usage
//     is set, and then only on the final chunk.
//   - Cached input tokens live under usage.prompt_tokens_details.cached_tokens.
//   - There is no cache breakpoint to place: OpenAI caches long prefixes
//     automatically, so Request.CacheSystem is a no-op here. Ordering still
//     matters, which is why Prompt puts the stable content first regardless of
//     which vendor the request lands on.
//   - Effort is Anthropic-only per DESIGN.md §8, so it is ignored rather than
//     mapped onto reasoning_effort, whose levels are a different set.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/RazerBlade6/apex/internal/provider"
)

// Name is the value that appears as `provider = "openai"` in config.toml.
const Name = "openai"

// schemaName labels the response format. OpenAI requires a name of
// [a-zA-Z0-9_-]{1,64}.
const schemaName = "apex_structured_output"

// Provider is the OpenAI adapter.
type Provider struct {
	client sdk.Client
	cfg    provider.Config
}

// New builds an adapter from a resolved credential and the defaults of one
// [models.*] entry. Key and base URL are injected for the same reasons as in
// the Anthropic adapter.
func New(cfg provider.Config) (*Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("openai: no API key was provided")
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

func (p *Provider) params(req provider.Request) (sdk.ChatCompletionNewParams, error) {
	req = p.cfg.Apply(req).WithDefaults()
	if err := req.Validate(); err != nil {
		return sdk.ChatCompletionNewParams{}, err
	}

	messages := make([]sdk.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if system := strings.TrimSpace(req.System); system != "" {
		messages = append(messages, sdk.SystemMessage(system))
	}
	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleAssistant:
			messages = append(messages, sdk.AssistantMessage(m.Content))
		default:
			messages = append(messages, sdk.UserMessage(m.Content))
		}
	}

	return sdk.ChatCompletionNewParams{
		Model:               shared.ChatModel(req.Model),
		Messages:            messages,
		MaxCompletionTokens: sdk.Int(int64(req.MaxTokens)),
		// Usage is omitted from a stream unless it is asked for, and Apex
		// needs it to confirm caching is working (DESIGN.md §8).
		StreamOptions: sdk.ChatCompletionStreamOptionsParam{IncludeUsage: sdk.Bool(true)},
	}, nil
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

// run drains one SSE stream onto ch. The deferred close is the only one, so
// the channel closes exactly once however this returns.
func (p *Provider) run(ctx context.Context, params sdk.ChatCompletionNewParams, ch chan<- provider.Event) {
	defer close(ch)

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close() //nolint:errcheck // nothing useful to do with a close failure here

	var usage provider.Usage
	for stream.Next() {
		chunk := stream.Current()
		if u, ok := usageOf(chunk); ok {
			usage = u
		}
		for _, choice := range chunk.Choices {
			if refusal := choice.Delta.Refusal; refusal != "" {
				provider.EmitFinal(ctx, ch, provider.Event{
					Type: provider.EventError,
					Err: &provider.Error{
						Provider: Name, Op: "stream", Kind: provider.KindRequest,
						Message: provider.Truncate("the model refused the request: " + refusal),
					},
				})
				return
			}
			if choice.Delta.Content == "" {
				continue
			}
			if !provider.Emit(ctx, ch, provider.Event{
				Type: provider.EventTextDelta,
				Text: choice.Delta.Content,
			}) {
				return // ctx is done; the caller is gone
			}
		}
	}

	if err := stream.Err(); err != nil {
		provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventError, Err: classify("stream", err)})
		return
	}
	provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventDone, Usage: &usage})
}

// Structured constrains the response to a JSON schema and unmarshals it.
//
// The schema is sent with strict enabled, which is what makes the guarantee
// worth having — without it the schema is a hint. Strict mode restricts what
// a schema may contain (object root, every property required,
// additionalProperties false); a schema that breaks those rules is rejected
// by the API with a message naming the offending field, which is the loud
// failure worth having over a silently unenforced schema.
func (p *Provider) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) error {
	if out == nil {
		return fmt.Errorf("openai: Structured needs a destination to unmarshal into")
	}
	params, err := p.params(req)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(schema, &decoded); err != nil {
		return fmt.Errorf("openai: output schema is not a JSON object: %w", err)
	}
	params.ResponseFormat = sdk.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   schemaName,
				Schema: decoded,
				Strict: sdk.Bool(true),
			},
		},
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close() //nolint:errcheck // nothing useful to do with a close failure here

	var body strings.Builder
	for stream.Next() {
		for _, choice := range stream.Current().Choices {
			if refusal := choice.Delta.Refusal; refusal != "" {
				return &provider.Error{
					Provider: Name, Op: "structured", Kind: provider.KindRequest,
					Message: provider.Truncate("the model refused the request: " + refusal),
				}
			}
			body.WriteString(choice.Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		return classify("structured", err)
	}
	if strings.TrimSpace(body.String()) == "" {
		return &provider.Error{
			Provider: Name, Op: "structured", Kind: provider.KindUnknown,
			Message: "the response carried no content",
		}
	}
	if err := json.Unmarshal([]byte(body.String()), out); err != nil {
		return fmt.Errorf("openai: decode structured response: %w", err)
	}
	return nil
}

// usageOf reads the usage block, which is present only on the final chunk and
// only when include_usage was set.
//
// CacheWriteTokens is left at zero, and that is the honest value rather than a
// gap: OpenAI's prompt caching is automatic and it does not bill or report a
// separate cache-write count, so there is no number to carry. Anthropic and
// claude-cli both report one, which is where DESIGN.md §8's argument for the
// field applies. The serving model comes from the chunk, since a dated
// snapshot id is what an undated request actually resolves to.
func usageOf(chunk sdk.ChatCompletionChunk) (provider.Usage, bool) {
	if !chunk.JSON.Usage.Valid() {
		return provider.Usage{}, false
	}
	u := chunk.Usage
	return provider.Usage{
		InputTokens:     int(u.PromptTokens),
		OutputTokens:    int(u.CompletionTokens),
		CacheReadTokens: int(u.PromptTokensDetails.CachedTokens),
		Model:           chunk.Model,
	}, true
}

// classify turns an SDK error into a classified provider.Error, on the same
// terms as the Anthropic adapter: status code when there was a response,
// transport error when there was not.
func classify(op string, err error) *provider.Error {
	out := &provider.Error{Provider: Name, Op: op, Err: err}

	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		out.StatusCode = apiErr.StatusCode
		out.Kind = provider.StatusKind(apiErr.StatusCode)
		switch {
		case apiErr.Message != "":
			out.Message = provider.Truncate(apiErr.Message)
		case apiErr.RawJSON() != "":
			out.Message = provider.Truncate(apiErr.RawJSON())
		default:
			out.Message = provider.Truncate(apiErr.Error())
		}
		return out
	}

	// An error frame arriving mid-stream is not an *openai.Error: the SDK has
	// already returned 200 and reports it as a plain error with no status. By
	// that point authentication and validation have both passed, so the
	// realistic causes are the vendor's own — which makes this the retryable
	// class rather than an unclassifiable one.
	if strings.Contains(err.Error(), streamErrorPrefix) {
		out.Kind = provider.KindServer
		out.Message = provider.Truncate(err.Error())
		return out
	}

	out.Kind = provider.TransportKind(err)
	out.Message = provider.Truncate(err.Error())
	return out
}

// streamErrorPrefix is what openai-go's SSE reader produces for an error frame
// inside an otherwise successful response. Matching on it is unpleasant, and
// the alternative — treating every such failure as unclassifiable — is worse.
const streamErrorPrefix = "received error while streaming"
