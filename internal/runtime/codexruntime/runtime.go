package codexruntime

import (
	"fmt"

	"baton/internal/codex"
	"baton/internal/config"
	"baton/internal/runtime"
	"baton/internal/tracker"
)

type Runtime struct {
	appServer *codex.AppServer
}

type session struct {
	session *codex.Session
}

func (session) isRuntimeSession() {}

func New(cfg *config.Config) runtime.Runtime {
	return &Runtime{appServer: codex.NewAppServer(cfg)}
}

func (r *Runtime) StartSession(workspace string) (runtime.Session, error) {
	sess, err := r.appServer.StartSession(workspace)
	if err != nil {
		return nil, err
	}
	return session{session: sess}, nil
}

func (r *Runtime) RunTurn(sess runtime.Session, prompt string, issue tracker.Issue, opts runtime.RunTurnOptions) (*runtime.TurnResult, error) {
	codexSession, ok := sess.(session)
	if !ok || codexSession.session == nil {
		return nil, fmt.Errorf("invalid runtime session type %T", sess)
	}
	return r.appServer.RunTurn(codexSession.session, prompt, issue, opts)
}

func (r *Runtime) StopSession(sess runtime.Session) {
	codexSession, ok := sess.(session)
	if !ok || codexSession.session == nil {
		return
	}
	r.appServer.StopSession(codexSession.session)
}
