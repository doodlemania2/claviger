package core

// Test seam of the erasure job (88-4): a hook called before each step, which
// can fail it to simulate a crash.
func SetErasureStepHook(fn func(req *Record, step string) error) { erasureStepHook = fn }

// ForgetErasureSecret drops the in-memory copy of the secret.
func ForgetErasureSecret(app App) { app.Store().Remove(erasureSecretStoreKey) }
