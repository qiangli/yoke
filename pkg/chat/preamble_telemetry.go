package chat

import (
	"context"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/telemetry"
)

func recordPreambleAdmission(ctx context.Context, p bus.PreparedPreamble) {
	r := p.AdmissionReport()
	if r.InputItems == 0 {
		return
	}
	telemetry.CoordinationAdmission(ctx, r.ContentDigest,
		int64(r.InputItems), int64(r.InputBytes), int64(r.AdmittedItems),
		int64(r.RenderedBytes), int64(r.OmittedItems), int64(r.OmittedBytes))
}
