package accounts

// RegisterDefaults registers every built-in adapter: one line per adapter,
// merged in story acceptance order.
func RegisterDefaults(r *Registry) error {
	if err := r.Register(NewLLMAdapter()); err != nil {
		return err
	}
	if err := r.Register(NewDigitalOceanAdapter()); err != nil {
		return err
	}
	if err := r.Register(NewAWSAdapter()); err != nil {
		return err
	}
	return nil
}
