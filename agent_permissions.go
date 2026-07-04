package claudeacp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/permissions"
)

func (a *Agent) permissionRulesForSession(ctx context.Context, sessionID acp.SessionId) (map[string]string, error) {
	a.mu.Lock()
	session := a.sessions[sessionID]
	a.mu.Unlock()

	if session != nil {
		return session.clonePermissionRules(), nil
	}

	return a.loadPermissionRules(ctx, sessionID)
}

func (a *Agent) cachedPermissionRules(sessionID acp.SessionId) (map[string]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rules, ok := a.permissionCache[sessionID]
	if !ok {
		return nil, false
	}

	return permissions.Clone(rules), true
}

func (a *Agent) cachePermissionRules(sessionID acp.SessionId, rules map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.permissionCache[sessionID] = permissions.Clone(rules)
}

func (a *Agent) deleteCachedPermissionRulesLocked(sessionID acp.SessionId) {
	delete(a.permissionCache, sessionID)
}

func (a *Agent) loadPermissionRules(ctx context.Context, sessionID acp.SessionId) (map[string]string, error) {
	store := permissions.Store{ClaudeHome: a.options.Home}

	rules, err := store.Load(ctx, string(sessionID))
	if err != nil {
		if cached, ok := a.cachedPermissionRules(sessionID); ok {
			a.log.WarnContext(ctx, "load permission rules failed; using cached rules", slog.String(jsonFieldError, err.Error()))

			return cached, nil
		}

		a.log.WarnContext(ctx, "load permission rules failed", slog.String(jsonFieldError, err.Error()))

		return nil, fmt.Errorf("load permission rules: %w", err)
	}

	a.cachePermissionRules(sessionID, rules)

	return rules, nil
}

func (a *Agent) permissionRulesForStart(
	ctx context.Context,
	id acp.SessionId,
	start sessionStart,
) (map[string]string, error) {
	if start.PermissionRules != nil {
		return start.PermissionRules, nil
	}

	return a.loadPermissionRules(ctx, id)
}
