package simulator

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"pm5-emulator/config"
	"pm5-emulator/sm"
)

const (
	tickInterval  = 100 * time.Millisecond
	splitDistance = 500.0 // meters; PM5 reports splits every 500m by default
	driveFraction = 0.35  // fraction of a stroke cycle spent in the drive
	newtonsPerLbf = 4.44822
)

// WorkoutConfig describes the workout a CSAFE client configured via
// SETTWORK/SETHORIZONTAL/SETPROGRAM/etc.
type WorkoutConfig struct {
	WorkoutType    byte
	DurationType   byte
	TargetDistance float64       // meters, 0 = unlimited ("just row")
	TargetDuration time.Duration // 0 = unlimited
	TargetPaceSec  float64       // seconds per 500m; 0 = keep current pace
}

// Simulator drives a physics-based "steady state with light jitter" row: it
// advances LiveState over time whenever the shared state machine reports
// INUSE, and freezes/report events (split boundaries, workout end) that the
// rowing service surfaces as characteristic notifications.
type Simulator struct {
	sm *sm.StateMachine

	mu    sync.RWMutex
	state LiveState

	workoutType    byte
	durationType   byte
	targetDistance float64
	targetDuration time.Duration
	targetSplitSec float64
	strokeRateSPM  float64
	dragFactor     byte

	lastSplitDistance float64
	lastSplitElapsed  time.Duration
	strokePhase       time.Duration

	rng *rand.Rand

	subsMu         sync.RWMutex
	subs           []*eventSubscription
	forceCurveSubs []chan []uint16
}

// eventSubscription delivers split-boundary and workout-finished events to
// one subscriber (typically one BLE notify goroutine). Channels are
// buffered(1) and sent to non-blockingly, so a slow/absent reader just
// coalesces events instead of stalling the simulator tick.
type eventSubscription struct {
	split   chan struct{}
	summary chan struct{}
}

// NewSimulator creates a Simulator bound to the device-wide state machine,
// with a default "Just Row" workout at a 2:00/500m pace.
func NewSimulator(machine *sm.StateMachine) *Simulator {
	s := &Simulator{
		sm:             machine,
		workoutType:    config.WORKOUTTYPE_JUSTROW_NOSPLITS,
		durationType:   config.CSAFE_TIME_DURATION,
		targetSplitSec: 120,
		strokeRateSPM:  24,
		dragFactor:     120,
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.resetLocked()
	return s
}

// Subscribe registers a new event listener and returns its split-boundary
// and workout-finished channels, plus a function to unregister it. Callers
// (typically a BLE notify goroutine) should call unsubscribe when they're
// done, e.g. once gatt.Notifier.Done() reports true.
func (s *Simulator) Subscribe() (split <-chan struct{}, summary <-chan struct{}, unsubscribe func()) {
	sub := &eventSubscription{split: make(chan struct{}, 1), summary: make(chan struct{}, 1)}

	s.subsMu.Lock()
	s.subs = append(s.subs, sub)
	s.subsMu.Unlock()

	unsubscribe = func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		for i, x := range s.subs {
			if x == sub {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
	}

	return sub.split, sub.summary, unsubscribe
}

func (s *Simulator) publishSplit() {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for _, sub := range s.subs {
		select {
		case sub.split <- struct{}{}:
		default:
		}
	}
}

func (s *Simulator) publishSummary() {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for _, sub := range s.subs {
		select {
		case sub.summary <- struct{}{}:
		default:
		}
	}
}

// SubscribeForceCurve registers a new force-curve event listener, returning
// its channel (one completed stroke's raw points per event) plus a function
// to unregister it. Separate from Subscribe/eventSubscription since only the
// Force Curve characteristic needs this one.
func (s *Simulator) SubscribeForceCurve() (points <-chan []uint16, unsubscribe func()) {
	ch := make(chan []uint16, 1)

	s.subsMu.Lock()
	s.forceCurveSubs = append(s.forceCurveSubs, ch)
	s.subsMu.Unlock()

	unsubscribe = func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		for i, x := range s.forceCurveSubs {
			if x == ch {
				s.forceCurveSubs = append(s.forceCurveSubs[:i], s.forceCurveSubs[i+1:]...)
				break
			}
		}
	}

	return ch, unsubscribe
}

func (s *Simulator) publishForceCurve(points []uint16) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for _, ch := range s.forceCurveSubs {
		select {
		case ch <- points:
		default:
		}
	}
}

// applyFrame overwrites the live state wholesale (used by the CSV player,
// which reconstructs a full snapshot per row rather than incrementally
// deriving one field from another like tick() does), firing the same
// split/summary events tick() would have fired had it produced this
// transition itself.
func (s *Simulator) applyFrame(next LiveState) {
	s.mu.Lock()
	prev := s.state
	s.state = next
	s.mu.Unlock()

	if next.LastSplitDistance != prev.LastSplitDistance || next.SplitIntervalNumber != prev.SplitIntervalNumber {
		s.publishSplit()
	}
	if next.WorkoutState == config.WORKOUTSTATE_WORKOUTEND && prev.WorkoutState != config.WORKOUTSTATE_WORKOUTEND {
		s.publishSummary()
	}
}

// ConfigureWorkout applies a new workout configuration (from a CSAFE
// SETTWORK/SETHORIZONTAL/SETPROGRAM/... command) and resets live counters.
func (s *Simulator) ConfigureWorkout(cfg WorkoutConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workoutType = cfg.WorkoutType
	s.durationType = cfg.DurationType
	s.targetDistance = cfg.TargetDistance
	s.targetDuration = cfg.TargetDuration
	if cfg.TargetPaceSec > 0 {
		s.targetSplitSec = cfg.TargetPaceSec
	}
	s.resetLocked()
}

// Reset zeroes the live workout counters (elapsed time, distance, calories,
// stroke count, ...) without touching the configured workout/pace.
func (s *Simulator) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

func (s *Simulator) resetLocked() {
	s.state = newLiveState()
	s.lastSplitDistance = 0
	s.lastSplitElapsed = 0
	s.strokePhase = 0
}

// newLiveState returns the correct "nothing happening yet" defaults: heart
// rate and its derivatives read as invalid (255, no belt paired), and
// interval type reads as NONE rather than 0 (which is a real interval type,
// INTERVALTYPE_TIME) -- sending 0 would make every "Just Row" workout look
// like an active time interval to clients.
func newLiveState() LiveState {
	return LiveState{
		HeartRate:          255,
		EndingHeartrate:    255,
		AvgHeartrate:       255,
		MinHeartrate:       255,
		MaxHeartrate:       255,
		RecoveryHeartRate:  255,
		SplitWorkHeartrate: 255,
		SplitRestHeartrate: 255,
		IntervalType:       config.INTERVALTYPE_NONE,
		SplitIntervalType:  config.INTERVALTYPE_NONE,
	}
}

// Snapshot returns a thread-safe copy of the current live state.
func (s *Simulator) Snapshot() LiveState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Run advances the simulation until the process exits. It's meant to be
// started once, in its own goroutine, for the lifetime of the emulator.
func (s *Simulator) Run() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	last := time.Now()
	for now := range ticker.C {
		dt := now.Sub(last)
		last = now
		s.tick(dt)
	}
}

func (s *Simulator) tick(dt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sm.GetState() != s.sm.INUSE {
		return
	}

	dtSec := dt.Seconds()
	splitSec := s.targetSplitSec * (1 + s.jitter(0.01))
	speed := 500.0 / splitSec
	power := paceToWatts(splitSec)
	calPerHr := 4*power + 300 // rough Concept2 watts->cal/hr approximation

	s.state.ElapsedTime += dt
	s.state.Distance += speed * dtSec
	s.state.TotalCalories += calPerHr * dtSec / 3600

	s.state.Speed = speed
	s.state.CurrentPace = paceFromSplitSeconds(splitSec)
	s.state.AvgPace = s.state.CurrentPace
	s.state.SplitAvgPace = s.state.CurrentPace
	s.state.AveragePower = power
	s.state.SplitAvgPower = power
	s.state.StrokePower = power
	s.state.StrokeCalories = calPerHr
	s.state.SplitAvgCalories = calPerHr
	s.state.SplitSpeed = speed
	s.state.SplitPower = power

	s.state.DragFactor = s.dragFactor
	s.state.DragFactorAvg = s.dragFactor
	s.state.SplitAvgDragFactor = s.dragFactor

	s.state.WorkoutType = s.workoutType
	s.state.WorkoutDurationType = s.durationType
	s.state.TotalWorkDistance = s.targetDistance
	s.state.WorkoutDuration = s.targetDuration
	s.state.WorkoutState = config.WORKOUTSTATE_WORKOUTROW
	s.state.RowingState = config.ROWINGSTATE_ACTIVE

	s.updateStroke(dt, speed, power)
	s.updateSplit()
	s.checkWorkoutComplete()
}

func (s *Simulator) updateStroke(dt time.Duration, speed, power float64) {
	strokeInterval := time.Duration(60.0 / s.strokeRateSPM * float64(time.Second))
	driveTime := time.Duration(driveFraction * float64(strokeInterval))

	s.strokePhase += dt
	if s.strokePhase < driveTime {
		s.state.StrokeState = config.STROKESTATE_DRIVING_STATE
	} else {
		s.state.StrokeState = config.STROKESTATE_RECOVERY_STATE
	}

	if s.strokePhase < strokeInterval {
		return
	}
	s.strokePhase -= strokeInterval

	s.state.StrokeCount++
	s.state.StrokeRate = byte(math.Round(s.strokeRateSPM))
	s.state.AvgStrokeRate = s.state.StrokeRate
	s.state.SplitAvgStrokeRate = s.state.StrokeRate
	s.state.DriveTime = driveTime
	s.state.StrokeRecoveryTime = strokeInterval - driveTime
	s.state.DriveLength = 1.3 // meters; plausible fixed handle travel
	s.state.StrokeDistance = speed * strokeInterval.Seconds()

	work := power * strokeInterval.Seconds() // joules for the whole stroke cycle
	s.state.WorkPerStroke = work
	avgForceN := work / s.state.DriveLength
	s.state.AvgDriveForce = avgForceN / newtonsPerLbf
	s.state.PeakDriveForce = s.state.AvgDriveForce * 1.4

	s.state.ProjectedWorkTime = s.targetDuration
	s.state.ProjectedWorkDistance = s.targetDistance
}

func (s *Simulator) updateSplit() {
	s.state.SplitIntervalTime = s.state.ElapsedTime - s.lastSplitElapsed
	s.state.SplitIntervalDistance = s.state.Distance - s.lastSplitDistance

	if s.state.SplitIntervalDistance < splitDistance {
		return
	}

	s.state.LastSplitTime = s.state.SplitIntervalTime
	s.state.LastSplitDistance = s.state.SplitIntervalDistance
	s.state.SplitIntervalNumber++
	s.state.IntervalCount = s.state.SplitIntervalNumber

	s.lastSplitDistance = s.state.Distance
	s.lastSplitElapsed = s.state.ElapsedTime

	s.publishSplit()
}

func (s *Simulator) checkWorkoutComplete() {
	done := (s.targetDistance > 0 && s.state.Distance >= s.targetDistance) ||
		(s.targetDuration > 0 && s.state.ElapsedTime >= s.targetDuration)
	if !done {
		return
	}

	s.state.WorkoutState = config.WORKOUTSTATE_WORKOUTEND
	s.state.RowingState = config.ROWINGSTATE_INACTIVE
	s.state.SplitIntervalSize = uint16(splitDistance)
	s.state.SplitIntervalCount = s.state.SplitIntervalNumber
	s.state.TotalRestDistance = 0
	if s.state.ElapsedTime > 0 {
		s.state.AvgCalories = s.state.TotalCalories / s.state.ElapsedTime.Hours()
	}

	_ = s.sm.Update(config.CSAFE_GOFINISHED_CMD)

	s.publishSummary()
}

// jitter returns a random value in [-amp, amp).
func (s *Simulator) jitter(amp float64) float64 {
	return (s.rng.Float64()*2 - 1) * amp
}

// paceToWatts converts a 500m split time (seconds) to power output (watts)
// using Concept2's published pace/power relationship: watts = 2.80 / pace^3,
// where pace is seconds-per-meter.
func paceToWatts(splitSec float64) float64 {
	pacePerMeter := splitSec / 500.0
	return 2.80 / math.Pow(pacePerMeter, 3)
}

func paceFromSplitSeconds(splitSec float64) time.Duration {
	return time.Duration(splitSec * float64(time.Second))
}
