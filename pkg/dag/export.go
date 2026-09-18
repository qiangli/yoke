// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import "github.com/spf13/cobra"

// NewDagCmd returns the `dag` cobra command — the host-agnostic entry point a
// front-end mounts (e.g. `bashy dag`). Execute() it with the host's
// stdin/stdout/stderr; recover the agent-meaningful exit code from the returned
// error via ExitCodeOf.
func NewDagCmd() *cobra.Command { return newDagCmd() }
