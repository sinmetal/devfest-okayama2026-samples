package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
)

// EventStreamRunner abstracts runner.Runner.Run for unit testing.
type EventStreamRunner interface {
	Run(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...runner.RunOption) iter.Seq2[*session.Event, error]
}

// CompanionRunner holds the ADK runner and session service for executing turns.
type CompanionRunner struct {
	AppName        string
	Agent          agent.Agent
	SessionService session.Service
	Runner         EventStreamRunner
}

// LoadRunner initializes the Gemini LLM model, character LlmAgent, and InMemoryRunner.
func LoadRunner(ctx context.Context, clientCfg *genai.ClientConfig, modelName string) (*CompanionRunner, error) {
	llmModel, err := gemini.NewModel(ctx, modelName, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create gemini model: %w", err)
	}
	a, err := NewCharacterAgent(llmModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create character agent: %w", err)
	}
	sessionSvc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "WakuWaku",
		Agent:          a,
		SessionService: sessionSvc,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK runner: %w", err)
	}
	return &CompanionRunner{
		AppName:        "WakuWaku",
		Agent:          a,
		SessionService: sessionSvc,
		Runner:         r,
	}, nil
}

// RunTurn executes a single conversation turn in an isolated temporary session,
// replaying the bounded history supplied by the browser and cleaning up the session afterwards.
func RunTurn(ctx context.Context, r *CompanionRunner, body ChatRequest) (string, *genai.GroundingMetadata, error) {
	userID := uuid.NewString()
	createResp, err := r.SessionService.Create(ctx, &session.CreateRequest{
		AppName: r.AppName,
		UserID:  userID,
	})
	if err != nil {
		return "", nil, fmt.Errorf("failed to create session: %w", err)
	}
	sess := createResp.Session
	defer func() {
		_ = r.SessionService.Delete(context.Background(), &session.DeleteRequest{
			AppName:   r.AppName,
			UserID:    userID,
			SessionID: sess.ID(),
		})
	}()

	// Replay the bounded history supplied by this browser, never another user's session.
	for _, turn := range body.History {
		ev := session.NewEvent(ctx, uuid.NewString())
		if turn.Role == "user" {
			ev.Author = "user"
		} else {
			ev.Author = r.Agent.Name()
		}
		ev.Content = &genai.Content{
			Role:  turn.Role,
			Parts: []*genai.Part{{Text: turn.Text}},
		}
		if err := r.SessionService.AppendEvent(ctx, sess, ev); err != nil {
			return "", nil, fmt.Errorf("failed to append history event: %w", err)
		}
	}

	userMsg := &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: body.Message}},
	}

	var textBuilder strings.Builder
	var grounding *genai.GroundingMetadata

	for ev, err := range r.Runner.Run(ctx, userID, sess.ID(), userMsg, agent.RunConfig{}) {
		if err != nil {
			return "", nil, err
		}
		if ev == nil {
			continue
		}
		if ev.ErrorCode != "" {
			return "", nil, errors.New("ADK model invocation failed")
		}
		if ev.GroundingMetadata != nil {
			grounding = ev.GroundingMetadata
		}
		if ev.IsFinalResponse() && ev.Content != nil {
			for _, part := range ev.Content.Parts {
				if part != nil && part.Text != "" && !part.Thought {
					textBuilder.WriteString(part.Text)
				}
			}
		}
	}

	return textBuilder.String(), grounding, nil
}
