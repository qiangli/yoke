package fleet

import "github.com/qiangli/coreutils/pkg/weavecli"

// The agent-CLI marker table is registry DATA that lives here; weavecli (in
// the certified coreutils, which never links this package) exposes the seam
// as a variable so its IsAgentDriven can consult it when fleet is linked.
func init() { weavecli.DetectTool = DetectTool }
