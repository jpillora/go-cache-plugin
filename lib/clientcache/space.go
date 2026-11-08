// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package clientcache

import (
	"errors"
	"syscall"
)

// IsCapacityError reports exhausted filesystem space or quota, including a
// full tmpfs. Cache reads can treat these failures as misses and recompile.
func IsCapacityError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
