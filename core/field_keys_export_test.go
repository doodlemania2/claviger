package core

// SetFieldKeyBatchHook installs the test seam called after every committed
// batch of a rotation or conversion (an error aborts the run like a crash).
func SetFieldKeyBatchHook(f func(batch int) error) { fieldKeyBatchHook = f }

// ResetFieldKeyring empties the process-wide data key cache, so that the next
// bootstrap has to unwrap the keys from the database like a fresh process.
func ResetFieldKeyring() {
	fieldKeys.mu.Lock()
	defer fieldKeys.mu.Unlock()
	fieldKeys.byID = map[string]*fieldKeyEntry{}
	fieldKeys.current = map[string]string{}
	fieldKeys.strict = map[string]bool{}
}
