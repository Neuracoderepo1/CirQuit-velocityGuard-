package risk

import (
	"sync"
	"time"

	"velocityguard/internal/money"
)

// sample is one recorded spend event.
type sample struct {
	t    time.Time
	cost money.Micros
}

// RollingWindow tracks spend samples and computes velocity (spend/sec) over
// a fixed lookback duration using a simple sliding-window sum. This is the
// in-process, Redis-free hot-path state described in the architecture
// (section 4/9): no external dependency on the decision path.
type RollingWindow struct {
	mu       sync.Mutex
	lookback time.Duration
	samples  []sample // ordered by time ascending
	now      func() time.Time
}

func NewRollingWindow(lookback time.Duration) *RollingWindow {
	return &RollingWindow{lookback: lookback, now: time.Now}
}

func (w *RollingWindow) Record(cost money.Micros) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, sample{t: w.now(), cost: cost})
	w.evictLocked()
}

func (w *RollingWindow) evictLocked() {
	cutoff := w.now().Add(-w.lookback)
	i := 0
	for i < len(w.samples) && w.samples[i].t.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.samples = w.samples[i:]
	}
}

// Sum returns total spend within the lookback window.
func (w *RollingWindow) Sum() money.Micros {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.evictLocked()
	var total money.Micros
	for _, s := range w.samples {
		total += s.cost
	}
	return total
}

// Count returns the number of samples (requests) within the lookback window.
func (w *RollingWindow) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.evictLocked()
	return len(w.samples)
}

// VelocityPerSec returns spend-per-second over the lookback window.
func (w *RollingWindow) VelocityPerSec() float64 {
	sum := w.Sum()
	secs := w.lookback.Seconds()
	if secs <= 0 {
		return 0
	}
	return sum.Float() / secs
}

// MultiWindowTracker keeps several RollingWindows (e.g. 10s, 60s, 5m) for
// the same key so acceleration (short-term velocity vs longer-term
// baseline) can be computed without external storage.
type MultiWindowTracker struct {
	Short   *RollingWindow // e.g. 10s — reacts fast to bursts
	Medium  *RollingWindow // e.g. 60s
	Long    *RollingWindow // e.g. 5m — smoother baseline
	Created time.Time

	mu       sync.Mutex
	baseline float64 // learned mean $/sec under normal conditions (EWMA)
	initOnce sync.Once
}

func NewMultiWindowTracker() *MultiWindowTracker {
	return NewMultiWindowTrackerWithWindows(10*time.Second, 60*time.Second, 5*time.Minute)
}

// NewMultiWindowTrackerWithWindows allows overriding window durations —
// production uses the spec defaults (10s/60s/5m); tests use compressed
// windows so recovery/decay scenarios don't require real-time sleeps.
func NewMultiWindowTrackerWithWindows(short, medium, long time.Duration) *MultiWindowTracker {
	return &MultiWindowTracker{
		Short:   NewRollingWindow(short),
		Medium:  NewRollingWindow(medium),
		Long:    NewRollingWindow(long),
		Created: time.Now(),
	}
}

func (t *MultiWindowTracker) Record(cost money.Micros) {
	t.Short.Record(cost)
	t.Medium.Record(cost)
	t.Long.Record(cost)

	// Update an EWMA baseline from the long window's velocity. Alpha chosen
	// so the baseline adapts over minutes, not seconds — it should
	// represent "normal", not react to the same spike it's supposed to
	// detect.
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.Long.VelocityPerSec()
	const alpha = 0.02
	t.initOnce.Do(func() { t.baseline = cur })
	t.baseline = alpha*cur + (1-alpha)*t.baseline
}

// Baseline returns the learned normal spend velocity ($/sec).
func (t *MultiWindowTracker) Baseline() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.baseline
}

// VelocityMultiplier compares short-window (current) velocity to the
// learned baseline. A value of 1.0 means "normal"; 90+ means a 90x spike.
func (t *MultiWindowTracker) VelocityMultiplier() float64 {
	baseline := t.Baseline()
	current := t.Short.VelocityPerSec()
	if baseline <= 0 {
		if current > 0 {
			return 1 // no baseline yet established; don't divide by zero into infinity
		}
		return 0
	}
	return current / baseline
}

// Acceleration compares short-window velocity to medium-window velocity —
// a positive value means spend is still ramping up right now, not just
// elevated.
func (t *MultiWindowTracker) Acceleration() float64 {
	short := t.Short.VelocityPerSec()
	medium := t.Medium.VelocityPerSec()
	return short - medium
}

// ProjectedExposure projects spend forward `horizon` using current velocity
// plus a linear extrapolation of acceleration, per spec section 10.
func (t *MultiWindowTracker) ProjectedExposure(currentExposure money.Micros, horizon time.Duration) money.Micros {
	v := t.Short.VelocityPerSec()
	a := t.Acceleration()
	secs := horizon.Seconds()

	// projected = current + v*t + 0.5*a*t^2, floored at v*t (don't let a
	// negative acceleration term drive projection below simple linear if
	// acceleration is decelerating hard — still report at least the
	// no-acceleration linear projection as a floor, since deceleration
	// trends are less reliable to extrapolate quadratically for a risk
	// ceiling).
	linear := v * secs
	withAccel := v*secs + 0.5*a*secs*secs
	proj := linear
	if withAccel > linear {
		proj = withAccel
	}
	if proj < 0 {
		proj = 0
	}
	return currentExposure + money.FromFloat(proj)
}
