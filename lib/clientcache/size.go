// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package clientcache

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseLimit accepts an integer byte count, decimal units (KB, MB, GB), or
// binary units (KiB, MiB, GiB). Zero disables the persistent local cache.
func ParseLimit(value string) (int64, error) {
	number := strings.ToUpper(strings.TrimSpace(value))
	multiplier := int64(1)
	for _, unit := range []struct {
		name string
		size int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1},
	} {
		if strings.HasSuffix(number, unit.name) {
			number = strings.TrimSpace(strings.TrimSuffix(number, unit.name))
			multiplier = unit.size
			break
		}
	}
	amount, err := strconv.ParseInt(number, 10, 64)
	if err != nil || amount < 0 || amount > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("expected a non-negative size such as 2GiB, 2GB, or 0")
	}
	return amount * multiplier, nil
}
