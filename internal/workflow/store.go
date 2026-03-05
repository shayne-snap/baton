package workflow

import (
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrStoreAlreadyStarted = errors.New("workflow_store_already_started")

const defaultStorePollInterval = 1 * time.Second

type Store struct {
	mu sync.RWMutex

	path       string
	pathSource func() string
	stamp      fileStamp
	definition *Definition

	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

type fileStamp struct {
	modTimeUnixNano int64
	sizeBytes       int64
	contentHash     uint64
}

func NewStore(path string) (*Store, error) {
	return NewStoreWithPathSource(path, nil)
}

func NewStoreWithPathSource(path string, pathSource func() string) (*Store, error) {
	cleanPath := filepath.Clean(path)
	definition, stamp, err := loadDefinitionAndStamp(cleanPath)
	if err != nil {
		return nil, err
	}

	return &Store{
		path:       cleanPath,
		pathSource: pathSource,
		stamp:      stamp,
		definition: cloneDefinition(definition),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}, nil
}

func (s *Store) Start() error {
	if s == nil {
		return errors.New("nil workflow store")
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrStoreAlreadyStarted
	}
	s.started = true
	s.mu.Unlock()

	go s.loop()
	return nil
}

func (s *Store) Close() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.mu.Unlock()

	<-s.doneCh
}

func (s *Store) Current() (*Definition, error) {
	if s == nil {
		return nil, errors.New("nil workflow store")
	}

	_ = s.reloadLocked()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.definition == nil {
		return nil, errors.New("missing workflow definition")
	}
	return cloneDefinition(s.definition), nil
}

func (s *Store) SetPath(path string) error {
	if s == nil {
		return errors.New("nil workflow store")
	}

	cleanPath := filepath.Clean(path)
	nextDefinition, nextStamp, err := loadDefinitionAndStamp(cleanPath)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = cleanPath
	s.definition = cloneDefinition(nextDefinition)
	s.stamp = nextStamp
	return nil
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

func (s *Store) ForceReload() error {
	if s == nil {
		return errors.New("nil workflow store")
	}
	return s.reloadLocked()
}

func (s *Store) loop() {
	defer close(s.doneCh)

	ticker := time.NewTicker(defaultStorePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			_ = s.reloadLocked()
		}
	}
}

func (s *Store) reloadLocked() error {
	s.mu.RLock()
	configuredPath := s.path
	path := configuredPath
	currentStamp := s.stamp
	pathSource := s.pathSource
	s.mu.RUnlock()

	if pathSource != nil {
		if sourcePath := strings.TrimSpace(pathSource()); sourcePath != "" {
			path = filepath.Clean(sourcePath)
		}
	}

	nextStamp, err := currentStampForPath(path)
	if err != nil {
		return err
	}

	if path == configuredPath {
		if nextStamp == currentStamp {
			return nil
		}
	}
	nextDefinition, _, err := loadDefinitionAndStamp(path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	s.definition = cloneDefinition(nextDefinition)
	s.stamp = nextStamp
	return nil
}

func loadDefinitionAndStamp(path string) (*Definition, fileStamp, error) {
	definition, err := LoadFile(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	stamp, err := currentStampForPath(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	return definition, stamp, nil
}

func currentStampForPath(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fileStamp{}, err
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write(content)
	return fileStamp{
		modTimeUnixNano: info.ModTime().UTC().UnixNano(),
		sizeBytes:       info.Size(),
		contentHash:     hasher.Sum64(),
	}, nil
}

func cloneDefinition(definition *Definition) *Definition {
	if definition == nil {
		return nil
	}
	cloned := &Definition{
		Config:         map[string]any{},
		PromptTemplate: definition.PromptTemplate,
	}
	if definition.Config != nil {
		cloned.Config = cloneValue(definition.Config).(map[string]any)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = cloneValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = cloneValue(item)
		}
		return copied
	default:
		return typed
	}
}
