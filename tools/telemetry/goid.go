package telemetry

import (
	"bytes"
	"runtime"
	"strconv"
)

// goroutineID returns the id of the calling goroutine, or 0 if unknown.
//
// Why this exists: PocketBase runs nearly every database query without a
// context.Context (dbx queries carry a nil context), so a query cannot find the
// request that issued it through its arguments. Request handling and the
// queries it issues run on the same goroutine, so the request span is parked
// under the goroutine id for the duration of the request and looked up by the
// database driver wrapper. It is only ever called while telemetry is active.
func goroutineID() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		if id, err := strconv.ParseUint(string(b[:i]), 10, 64); err == nil {
			return id
		}
	}
	return 0
}
