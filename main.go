package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"google.golang.org/genai"
)

const (
	defaultChatModel         = "gemini-3.8-flash"
	defaultTTSModel          = "gemini-3.8-flash-tts"
	defaultVoiceName         = "Leda"
	defaultSystemInstruction = "You are WakuWaku, a friendly AI companion. Reply in the user's language. Keep answers concise and natural for spoken conversation."
	defaultRequestTimeout    = 120 * time.Second
)

var standardVoices = []string{
	"Achernar", "Achird", "Algenib", "Algieba", "Alnilam", "Aoede",
	"Autonoe", "Callirrhoe", "Charon", "Despina", "Enceladus", "Erinome",
	"Fenrir", "Gacrux", "Iapetus", "Kore", "Laomedeia", "Leda", "Orus", "Puck",
	"Pulcherrima", "Rasalgethi", "Sadachbia", "Sadaltager", "Schedar", "Sulafat",
	"Umbriel", "Vindemiatrix", "Zephyr", "Zubenelgenubi",
}

var savedVoiceRegex = regexp.MustCompile(`^voice_[A-Za-z0-9_-]{1,128}$`)

// Turn represents a single message in the conversation history.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ChatRequest is the JSON request payload for POST /chat.
type ChatRequest struct {
	Message string `json:"message"`
	History []Turn `json:"history"`
}

// Source represents a grounding web citation.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// ChatResponse is the JSON response payload for POST /chat.
type ChatResponse struct {
	Response      string   `json:"response"`
	Sources       []Source `json:"sources"`
	GroundingHTML *string  `json:"grounding_html"`
}

// SpeechRequest is the JSON request payload for POST /tts.
type SpeechRequest struct {
	Text  string `json:"text"`
	Voice string `json:"voice"`
}

// ConfigResponse is the JSON response payload for GET /config.
type ConfigResponse struct {
	Voices       []string          `json:"voices"`
	DefaultVoice string            `json:"default_voice"`
	VoiceLabels  map[string]string `json:"voice_labels"`
}

// ContentGenerator abstracts genai.Client.Models.GenerateContent for testing.
type ContentGenerator interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

type genaiModelsAdapter struct {
	client *genai.Client
}

func (a *genaiModelsAdapter) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return a.client.Models.GenerateContent(ctx, model, contents, config)
}

// TurnExecutor abstracts RunTurn for testing.
type TurnExecutor func(ctx context.Context, r *CompanionRunner, body ChatRequest) (string, *genai.GroundingMetadata, error)

// App holds the server state, configuration, and dependencies.
type App struct {
	BaseDir           string
	ChatModel         string
	TTSModel          string
	SystemInstruction string
	CompanionMode     string
	Voices            []string
	DefaultVoice      string
	VoiceLabels       map[string]string
	RequestTimeout    time.Duration

	GenAI      ContentGenerator
	Runner     *CompanionRunner
	RunTurnFn  TurnExecutor
	ClientCfg  *genai.ClientConfig
}

// VoiceConfig builds the voice list, default voice, and custom voice labels from environment variables.
func buildVoiceConfig(defaultVoice, customVoiceLabel string) ([]string, string, map[string]string, error) {
	if defaultVoice == "" {
		defaultVoice = defaultVoiceName
	}
	voices := slices.Clone(standardVoices)
	labels := make(map[string]string)
	if !slices.Contains(voices, defaultVoice) {
		if !savedVoiceRegex.MatchString(defaultVoice) {
			return nil, "", nil, errors.New("GEMINI_TTS_VOICE must be a standard voice name or a saved voice_... ID")
		}
		voices = append(voices, defaultVoice)
		label := strings.TrimSpace(customVoiceLabel)
		if label == "" {
			label = "カスタム音声"
		}
		labels[defaultVoice] = label
	}
	return voices, defaultVoice, labels, nil
}

// detectADC is a package-level variable so tests can stub credential detection.
var detectADC = func(scopes []string) (*auth.Credentials, error) {
	return credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: scopes,
	})
}

// newGenAIClient is a package-level variable so tests can inspect ClientConfig.
var newGenAIClient = func(ctx context.Context, cfg *genai.ClientConfig) (ContentGenerator, error) {
	c, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &genaiModelsAdapter{client: c}, nil
}

// loadRunnerFn is a package-level variable so tests can stub ADK runner initialization.
var loadRunnerFn = LoadRunner

// NewApp creates and configures an App instance from environment variables.
func NewApp(ctx context.Context, baseDir string) (*App, error) {
	if baseDir == "" {
		baseDir = "."
	}
	_ = loadDotEnv(filepath.Join(baseDir, ".env"))

	voices, defaultVoice, voiceLabels, err := buildVoiceConfig(
		os.Getenv("GEMINI_TTS_VOICE"),
		os.Getenv("GEMINI_TTS_VOICE_NAME"),
	)
	if err != nil {
		return nil, err
	}

	chatModel := envOrDefault("GEMINI_MODEL", defaultChatModel)
	ttsModel := envOrDefault("GEMINI_TTS_MODEL", defaultTTSModel)
	sysInst := envOrDefault("SYSTEM_INSTRUCTION", defaultSystemInstruction)
	companionMode := envOrDefault("COMPANION_MODE", "gemini")

	app := &App{
		BaseDir:           baseDir,
		ChatModel:         chatModel,
		TTSModel:          ttsModel,
		SystemInstruction: sysInst,
		CompanionMode:     companionMode,
		Voices:            voices,
		DefaultVoice:      defaultVoice,
		VoiceLabels:       voiceLabels,
		RequestTimeout:    defaultRequestTimeout,
		RunTurnFn:         RunTurn,
	}

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" || project == "your-project-id" {
		log.Println("WARNING: Gemini authentication is not configured. See README.md.")
		return app, nil
	}

	creds, err := detectADC([]string{"https://www.googleapis.com/auth/cloud-platform"})
	if err != nil {
		log.Println("WARNING: Gemini authentication is not configured. See README.md.")
		return app, nil
	}

	timeout := 120 * time.Second
	attempts := int32(2)
	initialDelay := 1.0
	maxDelay := 2.0
	clientCfg := &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    envOrDefault("GOOGLE_CLOUD_LOCATION", "global"),
		Credentials: creds,
		HTTPOptions: genai.HTTPOptions{
			Timeout: &timeout,
			RetryOptions: &genai.HTTPRetryOptions{
				Attempts:        &attempts,
				InitialDelay:    &initialDelay,
				MaxDelay:        &maxDelay,
				HTTPStatusCodes: []int32{429, 500, 502, 503, 504},
			},
		},
	}
	app.ClientCfg = clientCfg

	genClient, err := newGenAIClient(ctx, clientCfg)
	if err != nil {
		log.Println("WARNING: Gemini authentication is not configured. See README.md.")
		return app, nil
	}
	app.GenAI = genClient

	runnerInstance, err := loadRunnerFn(ctx, clientCfg, chatModel)
	if err != nil {
		log.Printf("WARNING: Failed to initialize ADK runner: %v", err)
	} else {
		app.Runner = runnerInstance
	}

	return app, nil
}

// Handler returns the HTTP handler for all routes.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	staticDir := filepath.Join(a.BaseDir, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /config", a.handleConfig)
	mux.HandleFunc("POST /chat", a.handleChat)
	mux.HandleFunc("POST /tts", a.handleTTS)
	return mux
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(a.BaseDir, "templates", "index.html"))
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ConfigResponse{
		Voices:       a.Voices,
		DefaultVoice: a.DefaultVoice,
		VoiceLabels:  a.VoiceLabels,
	})
}

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := decodeAndValidateChatRequest(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if a.GenAI == nil {
		writeError(w, http.StatusServiceUnavailable, "Gemini authentication is not configured. See README.md.")
		return
	}

	if a.Runner != nil {
		ctx, cancel := context.WithTimeout(r.Context(), a.RequestTimeout)
		defer cancel()

		text, grounding, err := a.RunTurnFn(ctx, a.Runner, body)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeError(w, http.StatusGatewayTimeout, "エージェントの応答がタイムアウトしました。もう一度送信してください。")
				return
			}
			var apiErr genai.APIError
			if errors.As(err, &apiErr) {
				log.Printf("WARNING: ADK model request failed (code=%d)", apiErr.Code)
				status := http.StatusBadGateway
				if apiErr.Code == http.StatusTooManyRequests {
					status = http.StatusTooManyRequests
				}
				writeError(w, status, "Agent request failed. Check quota and model access.")
				return
			}
			log.Printf("WARNING: ADK request failed (%T)", err)
			writeError(w, http.StatusBadGateway, "Agent request failed. Check model access and server configuration.")
			return
		}
		if strings.TrimSpace(text) == "" {
			writeError(w, http.StatusBadGateway, "The agent returned no text. Please rephrase your message.")
			return
		}
		writeJSON(w, http.StatusOK, chatResult(text, grounding))
		return
	}

	if a.CompanionMode == "echo" {
		writeJSON(w, http.StatusOK, ChatResponse{
			Response: body.Message,
			Sources:  []Source{},
		})
		return
	}

	contents := make([]*genai.Content, 0, len(body.History)+1)
	for _, turn := range body.History {
		contents = append(contents, &genai.Content{
			Role:  turn.Role,
			Parts: []*genai.Part{{Text: turn.Text}},
		})
	}
	contents = append(contents, &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: body.Message}},
	})

	result, status, errMsg := a.generate(r.Context(), a.ChatModel, contents, &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: a.SystemInstruction}},
		},
		MaxOutputTokens: 2048,
		ThinkingConfig: &genai.ThinkingConfig{
			ThinkingLevel: genai.ThinkingLevelLow,
		},
	})
	if errMsg != "" {
		writeError(w, status, errMsg)
		return
	}

	text := result.Text()
	if strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadGateway, "Gemini returned no text. Please rephrase your message.")
		return
	}
	var grounding *genai.GroundingMetadata
	if len(result.Candidates) > 0 && result.Candidates[0] != nil {
		grounding = result.Candidates[0].GroundingMetadata
	}
	writeJSON(w, http.StatusOK, chatResult(text, grounding))
}

func (a *App) handleTTS(w http.ResponseWriter, r *http.Request) {
	body, err := decodeAndValidateSpeechRequest(r, a.DefaultVoice, a.Voices)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if a.GenAI == nil {
		writeError(w, http.StatusServiceUnavailable, "Gemini authentication is not configured. See README.md.")
		return
	}

	contents := []*genai.Content{
		{
			Role:  "user",
			Parts: []*genai.Part{{Text: body.Text}},
		},
	}
	config := &genai.GenerateContentConfig{
		ResponseModalities: []string{"AUDIO"},
		SpeechConfig: &genai.SpeechConfig{
			LanguageCode: "ja-JP",
			VoiceConfig: &genai.VoiceConfig{
				Voice: body.Voice,
			},
		},
	}

	result, status, errMsg := a.generate(r.Context(), a.TTSModel, contents, config)
	if errMsg != "" {
		writeError(w, status, errMsg)
		return
	}

	// Gemini 3.8 unary TTS returns a complete WAV container. Do not wrap it again.
	for _, candidate := range result.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part == nil || part.InlineData == nil {
				continue
			}
			data := part.InlineData.Data
			if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
				w.Header().Set("Content-Type", "audio/wav")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
		}
	}

	writeError(w, http.StatusBadGateway, "Gemini returned no WAV audio. Please try again.")
}

func (a *App) generate(parentCtx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, int, string) {
	ctx, cancel := context.WithTimeout(parentCtx, a.RequestTimeout)
	defer cancel()

	resp, err := a.GenAI.GenerateContent(ctx, model, contents, config)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, http.StatusGatewayTimeout, "Geminiの応答がタイムアウトしました。もう一度送信してください。"
		}
		var apiErr genai.APIError
		if errors.As(err, &apiErr) {
			log.Printf("WARNING: Gemini request failed (code=%d)", apiErr.Code)
			if apiErr.Code == http.StatusTooManyRequests {
				return nil, http.StatusTooManyRequests, "Gemini quota exceeded. Please try again later."
			}
			return nil, http.StatusBadGateway, "Gemini request failed. Check model access and server configuration."
		}
		// Never return SDK errors containing credentials or request details to the browser.
		log.Printf("WARNING: Gemini connection failed (%T)", err)
		return nil, http.StatusBadGateway, "Could not connect to Gemini. Please try again later."
	}
	if resp == nil {
		return nil, http.StatusBadGateway, "Could not connect to Gemini. Please try again later."
	}
	return resp, http.StatusOK, ""
}

func chatResult(text string, grounding *genai.GroundingMetadata) ChatResponse {
	sources := make([]Source, 0)
	var html *string
	if grounding != nil {
		for _, chunk := range grounding.GroundingChunks {
			if chunk != nil && chunk.Web != nil && chunk.Web.URI != "" {
				if strings.HasPrefix(chunk.Web.URI, "https://") || strings.HasPrefix(chunk.Web.URI, "http://") {
					title := chunk.Web.Title
					if title == "" {
						title = chunk.Web.URI
					}
					sources = append(sources, Source{
						Title: title,
						URL:   chunk.Web.URI,
					})
				}
			}
		}
		if grounding.SearchEntryPoint != nil && grounding.SearchEntryPoint.RenderedContent != "" {
			rendered := grounding.SearchEntryPoint.RenderedContent
			html = &rendered
		}
	}
	return ChatResponse{
		Response:      text,
		Sources:       sources,
		GroundingHTML: html,
	}
}

func validateBoundedText(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	n := utf8.RuneCountInString(trimmed)
	if n < 1 || n > 8000 {
		return "", errors.New("text must be between 1 and 8000 characters")
	}
	return trimmed, nil
}

func decodeAndValidateChatRequest(r *http.Request) (ChatRequest, error) {
	var raw struct {
		Message *string `json:"message"`
		History []struct {
			Role *string `json:"role"`
			Text *string `json:"text"`
		} `json:"history"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		return ChatRequest{}, fmt.Errorf("invalid request JSON: %w", err)
	}
	if raw.Message == nil {
		return ChatRequest{}, errors.New("message is required")
	}
	msg, err := validateBoundedText(*raw.Message)
	if err != nil {
		return ChatRequest{}, err
	}
	if len(raw.History) > 20 {
		return ChatRequest{}, errors.New("history must have at most 20 items")
	}
	history := make([]Turn, 0, len(raw.History))
	for _, h := range raw.History {
		if h.Role == nil || (*h.Role != "user" && *h.Role != "model") {
			return ChatRequest{}, errors.New("turn role must be 'user' or 'model'")
		}
		if h.Text == nil {
			return ChatRequest{}, errors.New("turn text is required")
		}
		t, err := validateBoundedText(*h.Text)
		if err != nil {
			return ChatRequest{}, err
		}
		history = append(history, Turn{
			Role: *h.Role,
			Text: t,
		})
	}
	return ChatRequest{
		Message: msg,
		History: history,
	}, nil
}

func decodeAndValidateSpeechRequest(r *http.Request, defaultVoice string, allowedVoices []string) (SpeechRequest, error) {
	var raw struct {
		Text  *string `json:"text"`
		Voice *string `json:"voice"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		return SpeechRequest{}, fmt.Errorf("invalid request JSON: %w", err)
	}
	if raw.Text == nil {
		return SpeechRequest{}, errors.New("text is required")
	}
	text, err := validateBoundedText(*raw.Text)
	if err != nil {
		return SpeechRequest{}, err
	}
	voice := defaultVoice
	if raw.Voice != nil {
		voice = *raw.Voice
	}
	if !slices.Contains(allowedVoices, voice) {
		return SpeechRequest{}, errors.New("Select a configured voice")
	}
	return SpeechRequest{
		Text:  text,
		Voice: voice,
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv reads a .env file and sets environment variables that are not already set.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return scanner.Err()
}

func main() {
	ctx := context.Background()
	app, err := NewApp(ctx, ".")
	if err != nil {
		log.Fatalf("Failed to configure application: %v", err)
	}
	port := envOrDefault("PORT", "8000")
	addr := "127.0.0.1:" + port
	log.Printf("Server listening on http://%s", addr)
	if err := http.ListenAndServe(addr, app.Handler()); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
