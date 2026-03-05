package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"baton/internal/agent"
	"baton/internal/config"
	"baton/internal/logging"
	"baton/internal/observability"
	"baton/internal/orchestrator"
	"baton/internal/statusdashboard"
	"baton/internal/tracker"
	"baton/internal/workflow"
	"baton/internal/workspace"

	"github.com/rs/zerolog"
)

type Options struct {
	WorkflowPath string
	LogsRoot     string
	Port         int
	Logger       zerolog.Logger
}

type Application struct {
	logger        zerolog.Logger
	workflowPath  string
	workflowStore *workflow.Store
	logsRoot      string
	config        *config.Config
	portOverride  *int
	orchestrator  *orchestrator.Orchestrator
}

func New(opts Options) (*Application, error) {
	workflowPath, err := resolveWorkflowPath(opts.WorkflowPath, os.Getwd)
	if err != nil {
		return nil, err
	}

	absWorkflowPath, err := filepath.Abs(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}

	workflowStore, err := workflow.NewStore(absWorkflowPath)
	if err != nil {
		return nil, err
	}
	definition, err := workflowStore.Current()
	if err != nil {
		return nil, err
	}

	typedConfig, err := config.FromWorkflow(absWorkflowPath, definition)
	if err != nil {
		return nil, err
	}

	workspaceManager := workspace.NewManager(typedConfig, opts.Logger)
	issueTracker := tracker.NewClient(typedConfig)
	logsRoot := logging.ResolveLogsRoot(opts.LogsRoot)
	agentRunner := agent.NewRunner(typedConfig, workspaceManager, issueTracker, agent.RunnerOptions{LogsRoot: logsRoot})
	orch := orchestrator.New(typedConfig, issueTracker, workspaceManager, agentRunner, opts.Logger)
	var portOverride *int
	if opts.Port >= 0 {
		port := opts.Port
		portOverride = &port
	}

	return &Application{
		logger:        opts.Logger,
		workflowPath:  absWorkflowPath,
		workflowStore: workflowStore,
		logsRoot:      logsRoot,
		config:        typedConfig,
		portOverride:  portOverride,
		orchestrator:  orch,
	}, nil
}

func resolveWorkflowPath(path string, getwd func() (string, error)) (string, error) {
	workflowPath := strings.TrimSpace(path)
	if workflowPath != "" {
		return workflowPath, nil
	}
	cwd, err := getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "WORKFLOW.md"), nil
}

func (a *Application) Run(ctx context.Context) error {
	a.logger.Info().Str("workflow_path", a.workflowPath).Msg("baton service starting")

	if a.workflowStore != nil {
		if err := a.workflowStore.Start(); err != nil && !errors.Is(err, workflow.ErrStoreAlreadyStarted) {
			return err
		}
		defer a.workflowStore.Close()
	}

	watcher, err := workflow.NewWatcher(a.workflowPath)
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Start(func(event workflow.ChangeEvent) {
		if a.workflowStore != nil {
			if err := a.workflowStore.ForceReload(); err != nil {
				a.logger.Error().
					Err(err).
					Str("workflow_path", a.workflowPath).
					Str("source", event.Source).
					Msg("failed to reload workflow after file change; keeping last known good configuration")
				return
			}
			definition, err := a.workflowStore.Current()
			if err != nil {
				a.logger.Error().
					Err(err).
					Str("workflow_path", a.workflowPath).
					Str("source", event.Source).
					Msg("failed to read workflow store after file change; keeping last known good configuration")
				return
			}
			if err := a.config.ReplaceFromWorkflow(a.workflowPath, definition); err != nil {
				a.logger.Error().
					Err(err).
					Str("workflow_path", a.workflowPath).
					Str("source", event.Source).
					Msg("failed to apply workflow after file change; keeping last known good configuration")
				return
			}
		} else if err := a.config.ReloadFromDisk(); err != nil {
			a.logger.Error().
				Err(err).
				Str("workflow_path", a.workflowPath).
				Str("source", event.Source).
				Msg("failed to reload workflow after file change; keeping last known good configuration")
			return
		}

		a.logger.Info().
			Str("workflow_path", a.workflowPath).
			Str("source", event.Source).
			Msg("workflow reloaded")

		if _, err := a.orchestrator.RequestRefresh(); err != nil && !errors.Is(err, orchestrator.ErrOrchestratorUnavailable) {
			a.logger.Warn().Err(err).Msg("failed to request orchestrator refresh after workflow reload")
		}
	}); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var boundPort *int
	if a.config.ObservabilityEnabled() {
		refreshInterval := time.Duration(a.config.ObservabilityRefreshMS()) * time.Millisecond
		if refreshInterval <= 0 {
			refreshInterval = time.Second
		}
		renderInterval := time.Duration(a.config.ObservabilityRenderIntervalMS()) * time.Millisecond
		if renderInterval <= 0 {
			renderInterval = 16 * time.Millisecond
		}

		dashboard := statusdashboard.New(statusdashboard.Options{
			Provider:        a.orchestrator,
			Config:          a.config,
			SnapshotTimeout: 15 * time.Second,
			RefreshInterval: refreshInterval,
			RenderInterval:  renderInterval,
			BoundPortFn: func() *int {
				return boundPort
			},
		})
		defer dashboard.RenderOfflineStatus()
		go dashboard.Run(runCtx)
	}

	shutdownServer := func(context.Context) error { return nil }
	serverErrCh := make(chan error, 1)
	serverStarted := false

	if port := a.effectiveServerPort(); port != nil {
		host := a.config.ServerHost()
		if strings.TrimSpace(host) == "" {
			host = "127.0.0.1"
		}
		addr := net.JoinHostPort(host, strconv.Itoa(*port))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
			port := tcpAddr.Port
			boundPort = &port
		}

		handler := observability.NewHandler(observability.HandlerOptions{
			Orchestrator:    a.orchestrator,
			SnapshotTimeout: 15 * time.Second,
			WorkspaceRoot:   a.config.WorkspaceRoot(),
			LogsRoot:        a.logsRoot,
		})
		server := &http.Server{Handler: handler}
		shutdownServer = server.Shutdown
		serverStarted = true

		a.logger.Info().
			Str("host", host).
			Int("port", *port).
			Msg("observability api listening")

		go func() {
			err := server.Serve(listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrCh <- err
				return
			}
			serverErrCh <- nil
		}()
	}

	orchErrCh := make(chan error, 1)
	go func() {
		orchErrCh <- a.orchestrator.Run(runCtx)
	}()

	if !serverStarted {
		return <-orchErrCh
	}

	select {
	case err := <-orchErrCh:
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = shutdownServer(shutdownCtx)
		shutdownCancel()
		_ = <-serverErrCh
		return err
	case err := <-serverErrCh:
		if err != nil {
			cancel()
			orchErr := <-orchErrCh
			if orchErr != nil {
				return orchErr
			}
			return err
		}
		return <-orchErrCh
	case <-runCtx.Done():
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = shutdownServer(shutdownCtx)
		shutdownCancel()
		_ = <-serverErrCh
		return <-orchErrCh
	}
}

func (a *Application) effectiveServerPort() *int {
	if a.portOverride != nil {
		port := *a.portOverride
		return &port
	}
	return a.config.ServerPort()
}
