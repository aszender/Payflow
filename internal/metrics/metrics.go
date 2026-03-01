package metrics

import "time"

func ObserveLatency(name string, d time.Duration) {
	_ = name
	_ = d
}
