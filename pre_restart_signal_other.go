//go:build !unix

package pocketbase

// bindPreRestartSignal is a no-op on non-unix platforms.
// Restart() itself is already unsupported on windows; on js/wasm it returns
// errors.ErrUnsupported.
func bindPreRestartSignal(_ *PocketBase) {}

// PreRestartSignalHookId is kept exported so consumers can reference it
// regardless of build target (e.g. removing the hook in tests).
const PreRestartSignalHookId = "pbPreRestartSignal"
