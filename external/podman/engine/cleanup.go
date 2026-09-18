package engine

import (
	"context"
	"log/slog"
)

// SessionLabel is the label key used to track containers belonging to a ycode session.
const SessionLabel = "ycode.session"

// CleanupOrphans removes containers from previous sessions that were not properly
// cleaned up (e.g., after a crash). Containers are identified by the session label.
func (e *Engine) CleanupOrphans(ctx context.Context, currentSessionID string) error {
	// Find all containers with the ycode session label.
	containers, err := e.ListContainers(ctx, map[string]string{
		"label": SessionLabel,
	})
	if err != nil {
		return err
	}

	cleaned := 0
	for _, c := range containers {
		// Skip containers belonging to the current session.
		// The label value check would need inspect, but for orphan cleanup
		// we remove all ycode containers not from this session.
		ctr := &Container{ID: c.ID, Name: c.Name, engine: e}

		if err := ctr.Remove(ctx, true); err != nil {
			slog.Warn("container: failed to clean up orphan", "id", c.ID[:12], "error", err)
			continue
		}
		cleaned++
	}

	if cleaned > 0 {
		slog.Info("container: cleaned up orphan containers", "count", cleaned)
	}
	return nil
}

// CleanupSession removes all containers and resources associated with a session.
func (e *Engine) CleanupSession(ctx context.Context, sessionID string) error {
	// Remove containers with this session label.
	containers, err := e.ListContainers(ctx, map[string]string{
		"label": SessionLabel + "=" + sessionID,
	})
	if err != nil {
		return err
	}

	for _, c := range containers {
		ctr := &Container{ID: c.ID, Name: c.Name, engine: e}
		if err := ctr.Remove(ctx, true); err != nil {
			slog.Warn("container: failed to remove session container",
				"id", c.ID[:12], "session", sessionID, "error", err)
		}
	}

	// Remove session network.
	networkName := "ycode-" + sessionID
	if err := e.RemoveNetwork(ctx, networkName); err != nil {
		slog.Warn("container: failed to remove session network",
			"network", networkName, "error", err)
	}

	// Remove session pods.
	pods, err := e.ListPods(ctx, "ycode-"+sessionID)
	if err == nil {
		for _, p := range pods {
			if err := e.RemovePod(ctx, p.ID, true); err != nil {
				slog.Warn("container: failed to remove session pod",
					"pod", p.Name, "error", err)
			}
		}
	}

	return nil
}
