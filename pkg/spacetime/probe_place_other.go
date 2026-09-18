//go:build !darwin && !linux && !windows

package spacetime

func gatewayHardwareSignals() ([]string, error) {
	return nil, ErrNotApplicable
}
