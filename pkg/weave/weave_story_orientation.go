package weave

import (
	"fmt"
	"strings"
)

func sprintOrientationError(s *weaveStory) error {
	goalMissing := s == nil || strings.TrimSpace(s.PrimaryGoal) == ""
	specMissing := s == nil || strings.TrimSpace(s.SpecRef) == ""
	switch {
	case goalMissing && specMissing:
		return fmt.Errorf("sprint requires a primary goal and master execution plan before take/start — set --primary-goal and --spec with `bashy sprint edit`")
	case goalMissing:
		return fmt.Errorf("sprint requires a primary goal before take/start — set --primary-goal with `bashy sprint edit`")
	case specMissing:
		return fmt.Errorf("sprint requires a master execution plan before take/start — set --spec with `bashy sprint edit`")
	default:
		return nil
	}
}

func sprintOrientationLine(s *weaveStory) string {
	line := fmt.Sprintf("primary goal: %s\nmaster plan: %s", strings.TrimSpace(s.PrimaryGoal), strings.TrimSpace(s.SpecRef))
	if s != nil && len(s.Directives) > 0 {
		line += "\n" + formatSprintDirectivesSection(s.ID, s.Directives)
	}
	return line
}
