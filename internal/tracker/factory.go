package tracker

import (
	"strings"

	"baton/internal/config"
)

func NewClient(cfg *config.Config) Client {
	if cfg == nil {
		return NewMemoryClient(cfg)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.TrackerKind())) {
	case "memory":
		return NewMemoryClient(cfg)
	case "jira":
		return NewJiraClient(cfg)
	case "feishu":
		return NewFeishuClient(cfg)
	default:
		return NewLinearClient(cfg)
	}
}
