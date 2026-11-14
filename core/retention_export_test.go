package core

// SetRetentionBatchHook installs the test seam called after every committed
// batch of a retention run (an error aborts the run like a crash would).
func SetRetentionBatchHook(f func(policyId string, batch int) error) { retentionBatchHook = f }
