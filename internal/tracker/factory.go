package tracker

import (
	"strings"

	"baton/internal/config"
)

func NewClient(cfg *config.Config) Client {
	if cfg == nil {
		return NewMemoryClient(cfg)
	}
	if strings.EqualFold(strings.TrimSpace(cfg.TrackerKind()), "memory") {
		return NewMemoryClient(cfg)
	}
	return NewLinearClient(cfg)
}
