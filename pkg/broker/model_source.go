package broker

import (
	"context"
	"fmt"
	"strings"
)

// DoorModel is an OpenAI-compatible model resolved through a shared door.
// Key and BaseURL may contain credentials: keep them in memory, never log them
// or persist them in a model catalog. Resolve again for each invocation.
type DoorModel struct {
	ModelID string
	BaseURL string
	Key     string
}

// ResolveDoorModel resolves MODEL@DOOR (or DOOR-door-MODEL) using the grant
// installed by llm door import. The grant itself is the model source, so import
// and share rotation need no second catalog or copied credential. The door
// validates the served model ID when called, allowing newly served IDs without
// another import. WSL uses the Windows store when it has no grant of its own.
//
// ok means the name selects a door source, even when resolution fails. Callers
// must propagate that failure rather than falling back to a local model pull.
func ResolveDoorModel(ctx context.Context, name string) (m DoorModel, ok bool, err error) {
	var model, source string
	if strings.Contains(name, "@") {
		model, source, _ = strings.Cut(name, "@")
	} else if strings.Contains(name, "-door-") {
		source, model, _ = strings.Cut(name, "-door-")
	} else {
		return DoorModel{}, false, nil
	}
	source = strings.ToLower(source)
	if model == "" || strings.TrimSpace(model) != model || !ValidPeerName(source) {
		return DoorModel{}, true, fmt.Errorf("invalid door model %q: use MODEL@DOOR", name)
	}
	_, env, _, err := envFrom(ctx, source, "", "")
	if err != nil {
		return DoorModel{}, true, err
	}
	if env["OPENAI_BASE_URL"] == "" || env["OPENAI_API_KEY"] == "" {
		return DoorModel{}, true, fmt.Errorf("door %s returned an incomplete model endpoint", source)
	}
	return DoorModel{ModelID: model, BaseURL: strings.TrimRight(env["OPENAI_BASE_URL"], "/"), Key: env["OPENAI_API_KEY"]}, true, nil
}
