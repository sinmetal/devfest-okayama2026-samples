package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/genai"
)

type mockContentGenerator struct {
	fn        func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
	lastModel string
	lastCts   []*genai.Content
	lastCfg   *genai.GenerateContentConfig
	callCount int
}

func (m *mockContentGenerator) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	m.callCount++
	m.lastModel = model
	m.lastCts = contents
	m.lastCfg = config
	if m.fn != nil {
		return m.fn(ctx, model, contents, config)
	}
	return nil, nil
}

func newTestApp(t *testing.T, mockGen *mockContentGenerator) *App {
	t.Helper()
	t.Setenv("GOOGLE_CLOUD_PROJECT", "your-project-id")
	t.Setenv("COMPANION_MODE", "gemini")
	t.Setenv("GEMINI_TTS_VOICE", "")
	t.Setenv("GEMINI_TTS_VOICE_NAME", "")

	app, err := NewApp(context.Background(), ".")
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	app.GenAI = mockGen
	return app
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch v := body.(type) {
	case string:
		buf.WriteString(v)
	default:
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode failed: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func getRequest(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestChatModelHistoryAndNoSharedSession(t *testing.T) {
	mockGen := &mockContentGenerator{
		fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Role:  "model",
							Parts: []*genai.Part{{Text: "こんにちは！"}},
						},
					},
				},
			}, nil
		},
	}
	app := newTestApp(t, mockGen)
	h := app.Handler()

	rec := postJSON(t, h, "/chat", map[string]any{
		"message": "続けて",
		"history": []map[string]string{
			{"role": "user", "text": "こんにちは"},
			{"role": "model", "text": "やあ"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if resp.Response != "こんにちは！" {
		t.Fatalf("expected 'こんにちは！', got %q", resp.Response)
	}
	if mockGen.lastModel != "gemini-3.8-flash" {
		t.Fatalf("expected model gemini-3.8-flash, got %q", mockGen.lastModel)
	}
	if mockGen.lastCfg.ThinkingConfig == nil || mockGen.lastCfg.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelLow {
		t.Fatalf("expected thinking level LOW")
	}
	if len(mockGen.lastCts) != 3 || mockGen.lastCts[0].Role != "user" || mockGen.lastCts[1].Role != "model" || mockGen.lastCts[2].Role != "user" {
		t.Fatalf("unexpected contents roles: %+v", mockGen.lastCts)
	}
	if mockGen.lastCts[2].Parts[0].Text != "続けて" {
		t.Fatalf("expected last message '続けて', got %q", mockGen.lastCts[2].Parts[0].Text)
	}

	rec2 := postJSON(t, h, "/chat", map[string]any{"message": "別のブラウザ"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	if len(mockGen.lastCts) != 1 {
		t.Fatalf("expected 1 content item for fresh request, got %d", len(mockGen.lastCts))
	}
}

func TestInvalidChat(t *testing.T) {
	mockGen := &mockContentGenerator{}
	app := newTestApp(t, mockGen)
	h := app.Handler()

	tooManyHistory := make([]map[string]string, 21)
	for i := range tooManyHistory {
		tooManyHistory[i] = map[string]string{"role": "user", "text": "x"}
	}

	cases := []any{
		map[string]any{},
		map[string]any{"message": " "},
		`{"message": 3}`,
		map[string]any{"message": strings.Repeat("x", 8001)},
		map[string]any{"message": "x", "history": []map[string]string{{"role": "system", "text": "x"}}},
		map[string]any{"message": "x", "history": tooManyHistory},
	}

	for i, c := range cases {
		rec := postJSON(t, h, "/chat", c)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("case %d: expected 422, got %d (%s)", i, rec.Code, rec.Body.String())
		}
	}
	if mockGen.callCount != 0 {
		t.Fatalf("expected 0 model calls on invalid chat, got %d", mockGen.callCount)
	}
}

func TestTTSReturnsWAVWithoutDoubleHeader(t *testing.T) {
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVE"), bytes.Repeat([]byte{0, 0}, 240)...)
	mockGen := &mockContentGenerator{
		fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Parts: []*genai.Part{
								{
									InlineData: &genai.Blob{
										MIMEType: "audio/wav",
										Data:     wav,
									},
								},
							},
						},
					},
				},
			}, nil
		},
	}
	app := newTestApp(t, mockGen)
	h := app.Handler()

	cfgRec := getRequest(t, h, "/config")
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /config, got %d", cfgRec.Code)
	}
	var cfg ConfigResponse
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal /config failed: %v", err)
	}
	if len(cfg.Voices) != 30 {
		t.Fatalf("expected 30 standard voices, got %d", len(cfg.Voices))
	}

	for _, voice := range cfg.Voices {
		rec := postJSON(t, h, "/tts", map[string]string{"text": "こんにちは", "voice": voice})
		if rec.Code != http.StatusOK {
			t.Fatalf("voice %s: expected 200, got %d", voice, rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), wav) {
			t.Fatalf("voice %s: wav bytes mismatch", voice)
		}
		if rec.Header().Get("Content-Type") != "audio/wav" {
			t.Fatalf("expected Content-Type audio/wav, got %q", rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("expected Cache-Control no-store, got %q", rec.Header().Get("Cache-Control"))
		}
		if mockGen.lastModel != "gemini-3.8-flash-tts" {
			t.Fatalf("expected model gemini-3.8-flash-tts, got %q", mockGen.lastModel)
		}
		if mockGen.lastCfg.SpeechConfig.VoiceConfig.Voice != voice {
			t.Fatalf("expected voice %q, got %q", voice, mockGen.lastCfg.SpeechConfig.VoiceConfig.Voice)
		}
		if mockGen.lastCfg.SpeechConfig.LanguageCode != "ja-JP" {
			t.Fatalf("expected language code ja-JP, got %q", mockGen.lastCfg.SpeechConfig.LanguageCode)
		}
	}

	badRec := postJSON(t, h, "/tts", map[string]string{"text": "hi", "voice": "unknown"})
	if badRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for unknown voice, got %d", badRec.Code)
	}
}

func TestConfiguredVoiceLabelDefaultAndTTS(t *testing.T) {
	cases := []struct {
		voice         string
		name          string
		expectedLabel string
		count         int
	}{
		{"Kore", "unused", "Kore", 30},
		{"voice_saved-anime", "アニメキャラ", "アニメキャラ", 31},
		{"voice_saved-anime", "", "カスタム音声", 31},
	}

	wav := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, tc := range cases {
		t.Run(tc.voice+"_"+tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_CLOUD_PROJECT", "your-project-id")
			t.Setenv("GEMINI_TTS_VOICE", tc.voice)
			t.Setenv("GEMINI_TTS_VOICE_NAME", tc.name)

			app, err := NewApp(context.Background(), ".")
			if err != nil {
				t.Fatalf("NewApp failed: %v", err)
			}
			mockGen := &mockContentGenerator{
				fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
					return &genai.GenerateContentResponse{
						Candidates: []*genai.Candidate{
							{
								Content: &genai.Content{
									Parts: []*genai.Part{
										{InlineData: &genai.Blob{MIMEType: "audio/wav", Data: wav}},
									},
								},
							},
						},
					}, nil
				},
			}
			app.GenAI = mockGen
			h := app.Handler()

			cfgRec := getRequest(t, h, "/config")
			var cfg ConfigResponse
			if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfg); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if len(cfg.Voices) != tc.count {
				t.Fatalf("expected %d voices, got %d", tc.count, len(cfg.Voices))
			}
			if cfg.DefaultVoice != tc.voice {
				t.Fatalf("expected default voice %q, got %q", tc.voice, cfg.DefaultVoice)
			}
			label := cfg.VoiceLabels[tc.voice]
			if label == "" {
				label = tc.voice
			}
			if label != tc.expectedLabel {
				t.Fatalf("expected label %q, got %q", tc.expectedLabel, label)
			}

			for _, payload := range []map[string]string{
				{"text": "こんにちは"},
				{"text": "こんにちは", "voice": tc.voice},
			} {
				rec := postJSON(t, h, "/tts", payload)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
					t.Fatalf("expected 200 wav, got %d", rec.Code)
				}
				if mockGen.lastCfg.SpeechConfig.VoiceConfig.Voice != tc.voice {
					t.Fatalf("expected voice %q, got %q", tc.voice, mockGen.lastCfg.SpeechConfig.VoiceConfig.Voice)
				}
			}

			mockGen.callCount = 0
			badRec := postJSON(t, h, "/tts", map[string]string{"text": "hi", "voice": "voice_not-configured"})
			if badRec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d", badRec.Code)
			}
			if mockGen.callCount != 0 {
				t.Fatalf("expected 0 model calls on unconfigured voice")
			}
		})
	}
}

func TestNoTextOrAudio(t *testing.T) {
	mockGen := &mockContentGenerator{
		fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return &genai.GenerateContentResponse{}, nil
		},
	}
	app := newTestApp(t, mockGen)
	h := app.Handler()

	if rec := postJSON(t, h, "/chat", map[string]string{"message": "hi"}); rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 from /chat, got %d", rec.Code)
	}
	if rec := postJSON(t, h, "/tts", map[string]string{"text": "hi"}); rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 from /tts, got %d", rec.Code)
	}
}

func TestUpstreamErrorsAreSanitized(t *testing.T) {
	cases := []struct {
		code     int
		expected int
	}{
		{403, 502},
		{404, 502},
		{429, 429},
		{503, 502},
	}

	for _, tc := range cases {
		mockGen := &mockContentGenerator{
			fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return nil, genai.APIError{
					Code:    tc.code,
					Message: "private upstream detail",
				}
			},
		}
		app := newTestApp(t, mockGen)
		h := app.Handler()

		rec := postJSON(t, h, "/chat", map[string]string{"message": "hi"})
		if rec.Code != tc.expected {
			t.Fatalf("code %d: expected %d, got %d", tc.code, tc.expected, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "private upstream detail") {
			t.Fatalf("expected private upstream detail to be sanitized, got %s", rec.Body.String())
		}
	}
}

func TestConnectionFailureAndTimeout(t *testing.T) {
	mockGen := &mockContentGenerator{
		fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}
	app := newTestApp(t, mockGen)
	h := app.Handler()

	rec := postJSON(t, h, "/tts", map[string]string{"text": "hi"})
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
}

func TestNoCredentialsAndStaticAssets(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "your-project-id")
	app, err := NewApp(context.Background(), ".")
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	h := app.Handler()

	if rec := postJSON(t, h, "/chat", map[string]string{"message": "hi"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when unauthenticated, got %d", rec.Code)
	}
	if rec := getRequest(t, h, "/"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /, got %d", rec.Code)
	}
	if rec := getRequest(t, h, "/static/app.js"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /static/app.js, got %d", rec.Code)
	}
	cfgRec := getRequest(t, h, "/config")
	var cfg ConfigResponse
	_ = json.Unmarshal(cfgRec.Body.Bytes(), &cfg)
	if cfg.DefaultVoice != defaultVoiceName {
		t.Fatalf("expected default voice %q, got %q", defaultVoiceName, cfg.DefaultVoice)
	}
}

func TestVertexADCIsExplicitEvenWithOldEnvironment(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "hands-on-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "global")
	t.Setenv("GOOGLE_GENAI_USE_VERTEXAI", "false")
	t.Setenv("GOOGLE_API_KEY", "unused-test-key")

	origADC := detectADC
	origNewClient := newGenAIClient
	origLoadRunner := loadRunnerFn
	defer func() {
		detectADC = origADC
		newGenAIClient = origNewClient
		loadRunnerFn = origLoadRunner
	}()

	fakeCreds := &auth.Credentials{}
	var capturedScopes []string
	detectADC = func(scopes []string) (*auth.Credentials, error) {
		capturedScopes = scopes
		return fakeCreds, nil
	}
	var capturedCfg *genai.ClientConfig
	newGenAIClient = func(ctx context.Context, cfg *genai.ClientConfig) (ContentGenerator, error) {
		capturedCfg = cfg
		return &mockContentGenerator{}, nil
	}
	loadRunnerFn = func(ctx context.Context, clientCfg *genai.ClientConfig, modelName string) (*CompanionRunner, error) {
		return &CompanionRunner{AppName: "WakuWaku"}, nil
	}

	app, err := NewApp(context.Background(), ".")
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	if getRequest(t, app.Handler(), "/config").Code != http.StatusOK {
		t.Fatalf("expected /config 200")
	}
	if capturedCfg == nil {
		t.Fatalf("expected ClientConfig to be captured")
	}
	if capturedCfg.Backend != genai.BackendVertexAI {
		t.Fatalf("expected BackendVertexAI, got %v", capturedCfg.Backend)
	}
	if capturedCfg.Project != "hands-on-project" {
		t.Fatalf("expected project hands-on-project, got %q", capturedCfg.Project)
	}
	if capturedCfg.Location != "global" {
		t.Fatalf("expected location global, got %q", capturedCfg.Location)
	}
	if capturedCfg.Credentials != fakeCreds {
		t.Fatalf("expected credentials to match fakeCreds")
	}
	if capturedCfg.APIKey != "" {
		t.Fatalf("expected empty APIKey, got %q", capturedCfg.APIKey)
	}
	if capturedCfg.HTTPOptions.Timeout == nil || *capturedCfg.HTTPOptions.Timeout != 120*time.Second {
		t.Fatalf("expected timeout 120s")
	}
	if capturedCfg.HTTPOptions.RetryOptions == nil || *capturedCfg.HTTPOptions.RetryOptions.Attempts != 2 {
		t.Fatalf("expected retry attempts 2")
	}
	if len(capturedScopes) != 1 || capturedScopes[0] != "https://www.googleapis.com/auth/cloud-platform" {
		t.Fatalf("unexpected ADC scopes: %v", capturedScopes)
	}
}

func TestMissingADCReturnsActionable503(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "hands-on-project")
	origADC := detectADC
	defer func() { detectADC = origADC }()

	detectADC = func(scopes []string) (*auth.Credentials, error) {
		return nil, errors.New("not configured")
	}

	app, err := NewApp(context.Background(), ".")
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	h := app.Handler()
	if getRequest(t, h, "/").Code != http.StatusOK {
		t.Fatalf("expected / 200")
	}
	if postJSON(t, h, "/chat", map[string]string{"message": "hi"}).Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /chat 503")
	}
}

func TestADKRouteUsesCharacterAndPreservesGrounding(t *testing.T) {
	mockGen := &mockContentGenerator{}
	app := newTestApp(t, mockGen)
	app.Runner = &CompanionRunner{AppName: "WakuWaku"}

	called := 0
	app.RunTurnFn = func(ctx context.Context, r *CompanionRunner, body ChatRequest) (string, *genai.GroundingMetadata, error) {
		called++
		return "キャラクター応答", nil, nil
	}

	rec := postJSON(t, app.Handler(), "/chat", map[string]string{"message": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ChatResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Response != "キャラクター応答" {
		t.Fatalf("expected 'キャラクター応答', got %q", resp.Response)
	}
	if called != 1 {
		t.Fatalf("expected RunTurnFn to be called once, got %d", called)
	}
	if mockGen.callCount != 0 {
		t.Fatalf("expected direct GenerateContent not to be called")
	}
}

func TestStalledChatReturnsTimeout(t *testing.T) {
	for _, useAgent := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "agent"}[useAgent], func(t *testing.T) {
			cancelled := false
			mockGen := &mockContentGenerator{
				fn: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
					<-ctx.Done()
					cancelled = true
					return nil, ctx.Err()
				},
			}
			app := newTestApp(t, mockGen)
			app.RequestTimeout = 10 * time.Millisecond
			if useAgent {
				app.Runner = &CompanionRunner{AppName: "WakuWaku"}
				app.RunTurnFn = func(ctx context.Context, r *CompanionRunner, body ChatRequest) (string, *genai.GroundingMetadata, error) {
					<-ctx.Done()
					cancelled = true
					return "", nil, ctx.Err()
				}
			}

			rec := postJSON(t, app.Handler(), "/chat", map[string]string{"message": "hi"})
			if rec.Code != http.StatusGatewayTimeout {
				t.Fatalf("expected 504, got %d", rec.Code)
			}
			bodyBytes, _ := io.ReadAll(rec.Body)
			if !strings.Contains(string(bodyBytes), "タイムアウト") {
				t.Fatalf("expected timeout message in detail, got %s", string(bodyBytes))
			}
			if !cancelled {
				t.Fatalf("expected stalled call to be cancelled")
			}
		})
	}
}
