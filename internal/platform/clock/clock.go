// Package clock owns the platform wall/monotonic clock seam. It depends only
// on time; HTTP logs call Now. It owns no persistence or business timestamps.
package clock

import "time"

func Now() time.Time { return time.Now() }
