package netid

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

// Watch reports the current network whenever it changes.
//
// The first value is delivered immediately. Changes are detected by reading
// the routing socket rather than polling, so a Wi-Fi switch or a dock being
// attached is noticed at once.
//
// A single network change emits a storm of routing messages (interface down,
// addresses removed, new addresses, new default route), so messages are
// debounced: we wait for the table to go quiet before taking a snapshot,
// otherwise we would fingerprint a half-configured network.
func Watch(ctx context.Context, debounce time.Duration) (<-chan Network, error) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return nil, err
	}

	out := make(chan Network, 1)
	raw := make(chan struct{}, 64)

	go func() {
		defer unix.Close(fd)
		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// A transient read error should not kill change detection.
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if n <= 0 {
				continue
			}
			select {
			case raw <- struct{}{}:
			default: // a burst is already pending; one wake-up is enough
			}
		}
	}()

	go func() {
		defer close(out)

		current := Snapshot(ctx)
		emit(ctx, out, current)

		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			<-timer.C
		}
		pending := false

		for {
			select {
			case <-ctx.Done():
				return

			case <-raw:
				if !pending {
					pending = true
					timer.Reset(debounce)
					continue
				}
				// Still settling: push the deadline out.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)

			case <-timer.C:
				pending = false
				next := Snapshot(ctx)
				if next.Fingerprint() != current.Fingerprint() {
					current = next
					emit(ctx, out, current)
				}
			}
		}
	}()

	return out, nil
}

func emit(ctx context.Context, ch chan<- Network, n Network) {
	select {
	case ch <- n:
	case <-ctx.Done():
	}
}
