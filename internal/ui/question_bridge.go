package ui

import (
	"context"

	"julia/internal/core"
)

// QuestionRequest bridges the AskUserQuestion and ReviewFindings tools to the
// terminal: Review is set for a review to triage, else Question is asked. Reply
// should have capacity one so cancellation cannot leave a responder blocked.
type QuestionRequest struct {
	Question core.Question
	Review   *core.Review
	Reply    chan core.Answer
}

// NewAsker creates a cancellable question bridge with no worker goroutines.
func NewAsker() (core.Asker, <-chan QuestionRequest) {
	ask, _, requests := NewBridge()
	return ask, requests
}

// NewBridge creates the question and review bridges over one channel, so the
// terminal shows one of them at a time and settles both the same way.
func NewBridge() (core.Asker, core.Triager, <-chan QuestionRequest) {
	requests := make(chan QuestionRequest)
	send := func(ctx context.Context, request QuestionRequest) (core.Answer, error) {
		select {
		case requests <- request:
		case <-ctx.Done():
			return core.Answer{}, ctx.Err()
		}
		select {
		case answer := <-request.Reply:
			// A reply that races a cancellation loses: the task is already over.
			if err := ctx.Err(); err != nil {
				return core.Answer{}, err
			}
			return answer, nil
		case <-ctx.Done():
			return core.Answer{}, ctx.Err()
		}
	}
	ask := func(ctx context.Context, question core.Question) (core.Answer, error) {
		return send(ctx, QuestionRequest{Question: question, Reply: make(chan core.Answer, 1)})
	}
	triage := func(ctx context.Context, review core.Review) (core.Answer, error) {
		return send(ctx, QuestionRequest{Review: &review, Reply: make(chan core.Answer, 1)})
	}
	return ask, triage, requests
}
