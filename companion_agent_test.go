package main

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
)

type fakeLLM struct {
	name string
}

func (f *fakeLLM) Name() string {
	return f.name
}

func (f *fakeLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

type recordingSessionService struct {
	inner        session.Service
	appended     []*session.Event
	deleteCalled int
}

func (s *recordingSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return s.inner.Create(ctx, req)
}

func (s *recordingSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.inner.Get(ctx, req)
}

func (s *recordingSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *recordingSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	s.deleteCalled++
	return s.inner.Delete(ctx, req)
}

func (s *recordingSessionService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	s.appended = append(s.appended, ev)
	return s.inner.AppendEvent(ctx, sess, ev)
}

type fakeStreamRunner struct {
	events []*session.Event
}

func (f *fakeStreamRunner) Run(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...runner.RunOption) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func TestCharacterAgentConfiguration(t *testing.T) {
	llm := &fakeLLM{name: "gemini-3.8-flash"}
	a, err := NewCharacterAgent(llm)
	if err != nil {
		t.Fatalf("NewCharacterAgent failed: %v", err)
	}
	if a.Name() != "companion_agent" {
		t.Fatalf("expected agent name companion_agent, got %q", a.Name())
	}
	if !strings.Contains(characterInstruction, "わく") {
		t.Fatalf("expected characterInstruction to contain 'わく'")
	}
	if !strings.Contains(characterInstruction, "google_search") {
		t.Fatalf("expected characterInstruction to mention google_search")
	}
}

func TestADKHistoryFinalResponseAndCleanup(t *testing.T) {
	ctx := context.Background()
	llm := &fakeLLM{name: "gemini-3.8-flash"}
	a, err := NewCharacterAgent(llm)
	if err != nil {
		t.Fatalf("NewCharacterAgent failed: %v", err)
	}

	recSvc := &recordingSessionService{inner: session.InMemoryService()}

	intermediateEvent := session.NewEvent(ctx, "inv-1")
	intermediateEvent.Partial = true
	intermediateEvent.Content = &genai.Content{
		Role:  "model",
		Parts: []*genai.Part{{Text: "Intermediate"}},
	}

	finalEvent := session.NewEvent(ctx, "inv-1")
	finalEvent.Partial = false
	finalEvent.Content = &genai.Content{
		Role: "model",
		Parts: []*genai.Part{
			{Text: "Thought", Thought: true},
			{Text: "Final"},
		},
	}

	compRunner := &CompanionRunner{
		AppName:        "WakuWaku",
		Agent:          a,
		SessionService: recSvc,
		Runner:         &fakeStreamRunner{events: []*session.Event{intermediateEvent, finalEvent}},
	}

	body := ChatRequest{
		Message: "next",
		History: []Turn{
			{Role: "user", Text: "hello"},
			{Role: "model", Text: "hi"},
		},
	}

	text, _, err := RunTurn(ctx, compRunner, body)
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if text != "Final" {
		t.Fatalf("expected 'Final', got %q", text)
	}
	if len(recSvc.appended) != 2 {
		t.Fatalf("expected 2 appended history events, got %d", len(recSvc.appended))
	}
	if recSvc.appended[0].Author != "user" {
		t.Fatalf("expected first history event author 'user', got %q", recSvc.appended[0].Author)
	}
	if recSvc.appended[1].Author != "companion_agent" {
		t.Fatalf("expected second history event author 'companion_agent', got %q", recSvc.appended[1].Author)
	}
	if recSvc.deleteCalled != 1 {
		t.Fatalf("expected session Delete to be called once, got %d", recSvc.deleteCalled)
	}
}

func TestGroundingMetadataSourcesAndSuggestions(t *testing.T) {
	metadata := &genai.GroundingMetadata{
		GroundingChunks: []*genai.GroundingChunk{
			{
				Web: &genai.GroundingChunkWeb{
					URI:   "https://example.com/news",
					Title: "News",
				},
			},
		},
		SearchEntryPoint: &genai.SearchEntryPoint{
			RenderedContent: "<div>Google Search</div>",
		},
	}

	res := chatResult("Answer", metadata)
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://example.com/news" || res.Sources[0].Title != "News" {
		t.Fatalf("unexpected sources: %+v", res.Sources)
	}
	if res.GroundingHTML == nil || *res.GroundingHTML != "<div>Google Search</div>" {
		t.Fatalf("unexpected grounding_html: %v", res.GroundingHTML)
	}
}
