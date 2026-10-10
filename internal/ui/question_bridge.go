package ui

import (
	"context"

	"jarvis/internal/core"
)

// QuestionRequest bridges the AskUserQuestion tool to the terminal. Reply
// should have capacity one so cancellation cannot leave a responder blocked.
type QuestionRequest struct {
	Question core.Question
	Reply    chan core.Answer
}

// NewAsker creates a cancellable question bridge with no worker goroutines.
func NewAsker() (core.Asker, <-chan QuestionRequest) {
	requests := make(chan QuestionRequest)
	return func(ctx context.Context, question core.Question) (core.Answer, error) {
		request := QuestionRequest{Question: question, Reply: make(chan core.Answer, 1)}
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
	}, requests
}
