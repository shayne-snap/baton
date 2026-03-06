package runtime

import (
	"context"
	"time"

	"baton/internal/tracker"
)

type Update struct {
	Event             string
	Timestamp         time.Time
	AppServerPID      string
	CodexAppServerPID string
	Payload           any
	Raw               string
	Decision          string
}

type TurnResult struct {
	Result    any
	SessionID string
	ThreadID  string
	TurnID    string
}

type MessageHandler func(Update)
type ToolExecutor func(ctx context.Context, tool string, arguments any) map[string]any

type RunTurnOptions struct {
	Context      context.Context
	OnMessage    MessageHandler
	ToolExecutor ToolExecutor
}

type Session interface{}

type Runtime interface {
	StartSession(workspace string) (Session, error)
	RunTurn(session Session, prompt string, issue tracker.Issue, opts RunTurnOptions) (*TurnResult, error)
	StopSession(session Session)
}
