package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIGatewayServiceForward_RejectsDisabledImageGenerationIntents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "image model",
			body: []byte(`{"model":"gpt-image-2","input":"draw"}`),
		},
		{
			name: "image tool",
			body: []byte(`{"model":"gpt-5.4","input":"draw","tools":[{"type":"image_generation"}]}`),
		},
		{
			name: "image tool choice",
			body: []byte(`{"model":"gpt-5.4","input":"draw","tool_choice":{"type":"image_generation"}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := newOpenAIImageGenerationControlTestService(upstream)
			c, recorder := newOpenAIImageGenerationControlTestContext(false, "unit-test-agent/1.0")
			account := newOpenAIImageGenerationControlTestAccount()

			result, err := svc.Forward(context.Background(), c, account, tt.body)

			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, "permission_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
			require.Nil(t, upstream.lastReq, "disabled image request must not reach upstream")
		})
	}
}

func TestOpenAIGatewayServiceForward_DisabledGroupAllowsTextOnlyResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_text","model":"gpt-5.4","usage":{"input_tokens":3,"output_tokens":2}}`)),
		},
	}
	svc := newOpenAIImageGenerationControlTestService(upstream)
	c, recorder := newOpenAIImageGenerationControlTestContext(false, "unit-test-agent/1.0")
	account := newOpenAIImageGenerationControlTestAccount()

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"write code","stream":false}`))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 0, result.ImageCount)
	require.NotNil(t, upstream.lastReq)
}

func TestOpenAIGatewayServiceForward_CodexImageInjectionRespectsGroupCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		allowImages   bool
		bridgeEnabled bool
		wantInjected  bool
	}{
		{name: "disabled group skips injection", allowImages: false, bridgeEnabled: true, wantInjected: false},
		{name: "enabled group skips injection by default", allowImages: true, bridgeEnabled: false, wantInjected: false},
		{name: "enabled group injects image tool when bridge enabled", allowImages: true, bridgeEnabled: true, wantInjected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_codex","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":1}}`)),
				},
			}
			svc := newOpenAIImageGenerationControlTestService(upstream)
			svc.cfg.Gateway.CodexImageGenerationBridgeEnabled = tt.bridgeEnabled
			c, _ := newOpenAIImageGenerationControlTestContext(tt.allowImages, "codex_cli_rs/0.98.0")
			account := newOpenAIImageGenerationControlTestAccount()

			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"write code","stream":false}`))

			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			hasImageTool := gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation")`).Exists()
			require.Equal(t, tt.wantInjected, hasImageTool)
			instructions := gjson.GetBytes(upstream.lastBody, "instructions").String()
			require.Equal(t, tt.wantInjected, strings.Contains(instructions, "image_generation"))
		})
	}
}

func TestOpenAIGatewayServiceForward_ExplicitImageToolWorksWithBridgeDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_explicit_image","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}`)),
		},
	}
	svc := newOpenAIImageGenerationControlTestService(upstream)
	c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.98.0")
	account := newOpenAIImageGenerationControlTestAccount()
	body := []byte(`{"model":"gpt-5.4","input":"draw","stream":false,"tools":[{"type":"image_generation","format":"jpeg"}]}`)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation")`).Exists())
	require.Equal(t, "jpeg", gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation").output_format`).String())
	require.False(t, gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation").format`).Exists())
	instructions := gjson.GetBytes(upstream.lastBody, "instructions").String()
	require.NotContains(t, instructions, "image_generation")
}

func TestOpenAIGatewayServiceForward_ChannelBridgeOverrideEnablesCodexInjection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_channel_bridge","model":"gpt-5.4","usage":{"input_tokens":1,"output_tokens":1}}`)),
		},
	}
	svc := newOpenAIImageGenerationControlTestService(upstream)
	groupID := int64(4242)
	svc.channelService = newOpenAIImageGenerationControlChannelService(groupID, &Channel{
		ID:     9001,
		Status: StatusActive,
		FeaturesConfig: map[string]any{
			featureKeyCodexImageGenerationBridge: map[string]any{PlatformOpenAI: true},
		},
	})
	c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.98.0")
	account := newOpenAIImageGenerationControlTestAccount()

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"write code","stream":false}`))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation")`).Exists())
	instructions := gjson.GetBytes(upstream.lastBody, "instructions").String()
	require.Contains(t, instructions, "image_generation")
}

func TestOpenAIGatewayService_CodexImageGenerationBridgeOverridePrecedence(t *testing.T) {
	groupID := int64(4242)

	tests := []struct {
		name    string
		global  bool
		channel *Channel
		account *Account
		want    bool
	}{
		{
			name:   "global default enables bridge",
			global: true,
			account: &Account{
				Platform: PlatformOpenAI,
			},
			want: true,
		},
		{
			name:   "channel true overrides disabled global",
			global: false,
			channel: &Channel{ID: 1, Status: StatusActive, FeaturesConfig: map[string]any{
				featureKeyCodexImageGenerationBridge: map[string]any{PlatformOpenAI: true},
			}},
			account: &Account{Platform: PlatformOpenAI},
			want:    true,
		},
		{
			name:   "channel false overrides enabled global",
			global: true,
			channel: &Channel{ID: 1, Status: StatusActive, FeaturesConfig: map[string]any{
				featureKeyCodexImageGenerationBridge: map[string]any{PlatformOpenAI: false},
			}},
			account: &Account{Platform: PlatformOpenAI},
			want:    false,
		},
		{
			name:   "account false overrides channel and global true",
			global: true,
			channel: &Channel{ID: 1, Status: StatusActive, FeaturesConfig: map[string]any{
				featureKeyCodexImageGenerationBridge: map[string]any{PlatformOpenAI: true},
			}},
			account: &Account{
				Platform: PlatformOpenAI,
				Extra:    map[string]any{featureKeyCodexImageGenerationBridge: false},
			},
			want: false,
		},
		{
			name:   "nested account true overrides channel false",
			global: false,
			channel: &Channel{ID: 1, Status: StatusActive, FeaturesConfig: map[string]any{
				featureKeyCodexImageGenerationBridge: map[string]any{PlatformOpenAI: false},
			}},
			account: &Account{
				Platform: PlatformOpenAI,
				Extra: map[string]any{
					PlatformOpenAI: map[string]any{"codex_image_generation_bridge_enabled": true},
				},
			},
			want: true,
		},
		{
			name:   "non openai account extra is ignored",
			global: false,
			account: &Account{
				Platform: PlatformAnthropic,
				Extra:    map[string]any{featureKeyCodexImageGenerationBridge: true},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newOpenAIImageGenerationControlTestService(&httpUpstreamRecorder{})
			svc.cfg.Gateway.CodexImageGenerationBridgeEnabled = tt.global
			if tt.channel != nil {
				svc.channelService = newOpenAIImageGenerationControlChannelService(groupID, tt.channel)
			}
			apiKey := &APIKey{GroupID: &groupID}

			got := svc.isCodexImageGenerationBridgeEnabled(context.Background(), tt.account, apiKey)

			require.Equal(t, tt.want, got)
		})
	}
}

func TestOpenAIGatewayServiceWSPayloadShouldBridgeImageGen(t *testing.T) {
	svc := newOpenAIImageGenerationControlTestService(&httpUpstreamRecorder{})

	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		// --- should trigger ---
		{"zh generate poster", `{"input":[{"type":"message","role":"user","content":"生成一张奶茶宣传海报"}]}`, true},
		{"zh draw avatar", `{"input":[{"type":"message","role":"user","content":"画个赛博朋克风格头像"}]}`, true},
		{"zh make cover image", `{"input":[{"type":"message","role":"user","content":"帮我做张封面图"}]}`, true},
		{"zh draw a generic image", `{"input":[{"type":"message","role":"user","content":"画一张图"}]}`, true},
		{"zh replace background", `{"input":[{"type":"message","role":"user","content":"把这张图换个背景"}]}`, true},
		{"en draw a picture", `{"input":[{"role":"user","content":[{"type":"input_text","text":"draw a picture of a cat"}]}]}`, true},
		{"en generate an image of", `{"input":[{"role":"user","content":[{"type":"input_text","text":"generate an image of a sunset over mountains"}]}]}`, true},
		{"en edit the photo", `{"input":[{"role":"user","content":[{"type":"input_text","text":"please edit the photo to remove the car"}]}]}`, true},
		{"input_image edit", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,xxx"},{"type":"input_text","text":"做成水彩风格"}]}]}`, true},

		// --- should NOT trigger ---
		{"zh generate chart", `{"input":[{"type":"message","role":"user","content":"生成测试图表数据"}]}`, false},
		{"zh analyze screenshot", `{"input":[{"type":"message","role":"user","content":"这张图里的代码逻辑怎么改"}]}`, false},
		{"zh generate icon component", `{"input":[{"type":"message","role":"user","content":"生成一个图标组件"}]}`, false},
		{"en render component", `{"input":[{"role":"user","content":[{"type":"input_text","text":"render the component again after the change"}]}]}`, false},
		{"en background color", `{"input":[{"role":"user","content":[{"type":"input_text","text":"change the background color to dark"}]}]}`, false},
		{"en update logo", `{"input":[{"role":"user","content":[{"type":"input_text","text":"update the logo in the header markup"}]}]}`, false},
		{"en bare image noun", `{"input":[{"role":"user","content":[{"type":"input_text","text":"the hero image tag is broken, fix it"}]}]}`, false},
		{"json body analysis", `{"input":[{"type":"message","role":"user","content":"这条 JSON body 里的 | ID: 被 cmd_inject 当成 shell 管道了"}]}`, false},
		{"tool-only turn", `{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`, false},
		{"empty input", `{"input":[]}`, false},

		// --- scope: only the latest user message decides (no follow-up cue) ---
		{"history image then code turn", `{"input":[{"type":"message","role":"user","content":"生成一张海报"},{"type":"function_call_output","call_id":"c1","output":"done"},{"type":"message","role":"user","content":"现在帮我重构这个函数"}]}`, false},

		// --- contextual follow-up: continuation cue + earlier image intent in same payload ---
		{"zh another poster after poster", `{"input":[{"type":"message","role":"user","content":"生成一张内蒙古的美食海报，顺便把食品名字，也要显示。"},{"type":"message","role":"assistant","content":"已生成内蒙古美食海报"},{"type":"message","role":"user","content":"不错，再来一张 湖北的。"}]}`, true},
		{"en another after image", `{"input":[{"role":"user","content":[{"type":"input_text","text":"generate an image of a cat"}]},{"type":"message","role":"assistant","content":"done"},{"role":"user","content":[{"type":"input_text","text":"nice, another one please"}]}]}`, true},
		{"zh followup cue without prior image", `{"input":[{"type":"message","role":"user","content":"不错，再来一张"}]}`, false},
		{"zh coding turn after prior image", `{"input":[{"type":"message","role":"user","content":"生成一张海报"},{"type":"message","role":"assistant","content":"done"},{"type":"message","role":"user","content":"帮我写段 Go 代码"}]}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// WSPayloadShouldBridgeImageGen is the production gate: a turn is bridged only
			// when its latest user message expresses an image generation/edit intent
			// (tool-only turns and bare follow-up cues without prior image intent excluded).
			require.Equal(t, tt.want, svc.WSPayloadShouldBridgeImageGen([]byte(tt.payload)))
		})
	}
}

func TestOpenAIGatewayServiceApplyCodexImageGenerationBridgeToWSPayload_PreservesInstructions(t *testing.T) {
	svc := newOpenAIImageGenerationControlTestService(&httpUpstreamRecorder{})
	svc.cfg.Gateway.CodexImageGenerationBridgeEnabled = true
	_, _ = newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.98.0")
	groupID := int64(4242)
	apiKey := &APIKey{
		GroupID: &groupID,
		Group:   &Group{ID: groupID, AllowImageGeneration: true},
	}
	account := newOpenAIImageGenerationControlTestAccount()
	payload := []byte(`{"model":"gpt-5.5","instructions":"original codex instructions","input":[]}`)

	injected, modified := svc.ApplyCodexImageGenerationBridgeToWSPayload(
		context.Background(),
		"codex_cli_rs/0.98.0",
		"",
		payload,
		account,
		apiKey,
	)

	require.True(t, modified)
	require.True(t, gjson.GetBytes(injected, `tools.#(type=="image_generation")`).Exists())
	instructions := gjson.GetBytes(injected, "instructions").String()
	require.Contains(t, instructions, "original codex instructions")
	require.Contains(t, instructions, codexImageGenerationBridgeMarker)
}

func TestOpenAIGatewayServiceHandleResponsesImageOutputs_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := newOpenAIImageGenerationControlTestService(&httpUpstreamRecorder{})
	c, _ := newOpenAIImageGenerationControlTestContext(true, "unit-test-agent/1.0")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"resp_image_json",
			"model":"gpt-5.4",
			"output":[{"id":"ig_json_1","type":"image_generation_call","result":"final-image"}],
			"usage":{"input_tokens":7,"output_tokens":3,"output_tokens_details":{"image_tokens":2}}
		}`)),
	}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1, Type: AccountTypeAPIKey}, "gpt-5.4", "gpt-5.4")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.imageCount)
	require.NotNil(t, result.usage)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Equal(t, 2, result.usage.ImageOutputTokens)
}

func TestOpenAIGatewayServiceHandleResponsesImageOutputs_Streaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := newOpenAIImageGenerationControlTestService(&httpUpstreamRecorder{})
	c, _ := newOpenAIImageGenerationControlTestContext(true, "unit-test-agent/1.0")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_stream_1\",\"type\":\"image_generation_call\",\"result\":\"final-image\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_image_stream\",\"model\":\"gpt-5.5\",\"output\":[{\"id\":\"ig_stream_1\",\"type\":\"image_generation_call\",\"result\":\"final-image\"}],\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"output_tokens_details\":{\"image_tokens\":4}}}}\n\n",
		)),
	}

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "gpt-5.5", "gpt-5.5")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.imageCount)
	require.NotNil(t, result.usage)
	require.Equal(t, 11, result.usage.InputTokens)
	require.Equal(t, 5, result.usage.OutputTokens)
	require.Equal(t, 4, result.usage.ImageOutputTokens)
}

func newOpenAIImageGenerationControlTestService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	cfg := &config.Config{}
	return &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}
}

func newOpenAIImageGenerationControlChannelService(groupID int64, ch *Channel) *ChannelService {
	svc := &ChannelService{}
	cache := newEmptyChannelCache()
	if ch != nil {
		cache.channelByGroupID[groupID] = ch
		cache.byID[ch.ID] = ch
	}
	cache.loadedAt = time.Now()
	svc.cache.Store(cache)
	return svc
}

func newOpenAIImageGenerationControlTestContext(allowImages bool, userAgent string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", userAgent)
	groupID := int64(4242)
	c.Set("api_key", &APIKey{
		ID:      2424,
		GroupID: &groupID,
		Group: &Group{
			ID:                   groupID,
			AllowImageGeneration: allowImages,
			RateMultiplier:       1,
			ImageRateMultiplier:  1,
		},
	})
	return c, recorder
}

func newOpenAIImageGenerationControlTestAccount() *Account {
	return &Account{
		ID:          5151,
		Name:        "openai-image-controls",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}
}
