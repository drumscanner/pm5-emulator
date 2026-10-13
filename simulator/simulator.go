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
// SETTWORK/SETHORIZONTAL/SETPROGRAM/etc. (flat fields, a single goal) or via
// the proprietary SETPMCFG workout-programming commands (Intervals, a
// schedule of one or more work/rest goals).
type WorkoutConfig struct {
	WorkoutType    byte
	DurationType   byte
	TargetDistance float64       // meters, 0 = unlimited ("just row")
	TargetDuration time.Duration // 0 = unlimited
	TargetPaceSec  float64       // seconds per 500m; 0 = keep current pace

	// Intervals, if non-empty, makes this an interval workout: the rower
	// keeps rowing through each interval's rest (distance/time/calories
	// keep accumulating -- only the reported workout/interval state
	// changes), advancing through Intervals in order. Repeat cycles back
	// to the first one indefinitely (until Terminate) instead of finishing
	// after the last one -- "single"/"constant repeatable" workouts with
	// no explicit count use this to match a real PM5 repeating until
	// stopped; "variable repeatable" and any workout with an explicit
	// finite count do not.
	Intervals []IntervalSpec
	Repeat    bool
}

// IntervalSpec is one interval's work goal and the rest that follows it.
type IntervalSpec struct {
	DurationType   byte // config.CSAFE_TIME_DURATION / CSAFE_DISTANCE_DURATION / CSAFE_CALORIES_DURATION
	TargetDuration time.Duration
	TargetDistance float64
	TargetCalories float64
	RestDuration   time.Duration
	RestUndefined  bool // no set rest length; the emulator rests for undefinedRestDuration
}

// How long a rest without a set length lasts here; a real PM5 waits for the rower to start again.
const undefinedRestDuration = 10 * time.Second

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

	// Interval workouts: the configured schedule, which interval is
	// current, and the current work/rest phase's own progress.
	schedule    []IntervalSpec
	scheduleIdx int
	repeat      bool
	phase       intervalPhase

	rng *rand.Rand

	subsMu         sync.RWMutex
	subs           []*eventSubscription
	forceCurveSubs []chan []uint16

	restartMu sync.Mutex
	restartCh chan struct{}
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
		restartCh:      make(chan struct{}),
	}
	s.resetLocked()
	return s
}

// RequestRestart signals any in-progress CSV playback pass (see
// CSVTimeline.playOnce) to abort immediately and restart from the first
// stroke, e.g. when a new client connects mid-playback. Polling the state
// machine alone isn't reliable for this: a Reset()-then-straight-back-to-
// INUSE transition (as autoStartWorkout does) can happen entirely inside
// one of playOnce's real-time sleeps between frames and never be observed.
func (s *Simulator) RequestRestart() {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	close(s.restartCh)
	s.restartCh = make(chan struct{})
}

// restartSignal returns the channel that RequestRestart closes to broadcast
// a restart. Callers should re-fetch it after it fires, since it's replaced
// on every call to RequestRestart.
func (s *Simulator) restartSignal() <-chan struct{} {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	return s.restartCh
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
	s.schedule = cfg.Intervals
	s.repeat = cfg.Repeat
	if cfg.TargetPaceSec > 0 {
		s.targetSplitSec = cfg.TargetPaceSec
	}
	s.resetLocked()
}

// intervalSchedule returns a thread-safe snapshot of the configured interval
// schedule (nil/empty if this isn't an interval workout) and whether it
// repeats, for callers -- the CSV player -- that aren't otherwise
// synchronized with ConfigureWorkout. s.schedule is always replaced
// wholesale (never mutated in place), so returning the slice itself is safe.
func (s *Simulator) intervalSchedule() (schedule []IntervalSpec, repeat bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schedule, s.repeat
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
	s.scheduleIdx = 0
	s.phase = intervalPhase{}
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
	if len(s.schedule) > 0 {
		fields, scheduleDone := advanceIntervalPhase(&s.phase, s.schedule, &s.scheduleIdx, s.repeat,
			s.state.ElapsedTime, s.state.Distance, s.state.TotalCalories)
		s.state.WorkoutState = fields.WorkoutState
		s.state.IntervalType = fields.IntervalType
		s.state.RestTime = fields.RestTime
		s.state.RestDistance = fields.RestDistance
		s.state.IntervalCount = fields.IntervalCount
		if scheduleDone {
			s.finishWorkoutLocked()
		}
		return
	}
	s.updateSplit()
	s.checkWorkoutComplete()
}

// intervalPhase tracks progress through the current work or rest phase of an
// interval schedule. It's shared state for advanceIntervalPhase, used both by
// Simulator.tick (the physics engine) and CSVTimeline.playOnce (the CSV
// player), so the two data sources drive identical work/rest transitions
// from exactly the same logic instead of two implementations that could
// drift apart.
type intervalPhase struct {
	inRest        bool
	intervalCount byte
	phaseElapsed  time.Duration
	phaseDistance float64
	phaseCalories float64
}

// intervalFields is the subset of LiveState that advanceIntervalPhase
// computes from an interval schedule.
type intervalFields struct {
	WorkoutState  byte
	IntervalType  byte
	RestTime      time.Duration
	RestDistance  float64
	IntervalCount byte
}

// advanceIntervalPhase runs the work and rest phases of an interval
// schedule. Time, distance and calories keep accumulating through the rest,
// as the rower goes on rowing; the rest only changes the reported workout
// state and interval type, and counts its own time and distance (via the
// returned RestTime/RestDistance).
//
// elapsed/distance/calories are the CUMULATIVE values since the whole
// workout started (ps's own phaseElapsed/phaseDistance/phaseCalories track
// where the *current* phase began, exactly as Simulator.tick's caller
// already did before this was extracted). scheduleDone is true only once,
// the moment the schedule is exhausted and !repeat -- the caller is
// responsible for reacting to it (ending the workout); a repeating schedule
// never returns it.
func advanceIntervalPhase(ps *intervalPhase, schedule []IntervalSpec, scheduleIdx *int, repeat bool,
	elapsed time.Duration, distance, calories float64) (fields intervalFields, scheduleDone bool) {
	if len(schedule) == 0 {
		return intervalFields{}, false
	}
	if ps.intervalCount == 0 {
		ps.intervalCount = 1
	}
	spec := schedule[*scheduleIdx]

	phaseTime := elapsed - ps.phaseElapsed
	phaseDistance := distance - ps.phaseDistance
	phaseCalories := calories - ps.phaseCalories

	if ps.inRest {
		fields.WorkoutState = config.WORKOUTSTATE_INTERVALREST
		fields.IntervalType = config.INTERVALTYPE_REST
		fields.RestTime = phaseTime
		fields.RestDistance = phaseDistance
		rest := spec.RestDuration
		if spec.RestUndefined {
			fields.IntervalType = config.INTERVALTYPE_RESTUNDEFINED
			rest = undefinedRestDuration
		}
		fields.IntervalCount = ps.intervalCount

		if phaseTime >= rest {
			ps.inRest = false
			ps.intervalCount++
			ps.phaseElapsed, ps.phaseDistance, ps.phaseCalories = elapsed, distance, calories

			*scheduleIdx++
			if *scheduleIdx >= len(schedule) {
				if !repeat {
					return fields, true
				}
				*scheduleIdx = 0
			}
		}
		return fields, false
	}

	switch spec.DurationType {
	case config.CSAFE_DISTANCE_DURATION:
		fields.WorkoutState = config.WORKOUTSTATE_INTERVALWORKDISTANCE
		fields.IntervalType = config.INTERVALTYPE_DIST
	case config.CSAFE_CALORIES_DURATION:
		fields.WorkoutState = config.WORKOUTSTATE_INTERVALWORKDISTANCE
		fields.IntervalType = config.INTERVALTYPE_CAL
	default:
		fields.WorkoutState = config.WORKOUTSTATE_INTERVALWORKTIME
		fields.IntervalType = config.INTERVALTYPE_TIME
	}
	if spec.RestUndefined {
		switch fields.IntervalType {
		case config.INTERVALTYPE_TIME:
			fields.IntervalType = config.INTERVALTYPE_TIMERESTUNDEFINED
		case config.INTERVALTYPE_DIST:
			fields.IntervalType = config.INTERVALTYPE_DISTANCERESTUNDEFINED
		case config.INTERVALTYPE_CAL:
			fields.IntervalType = config.INTERVALTYPE_CALRESTUNDEFINED
		}
	}
	fields.IntervalCount = ps.intervalCount

	done := false
	switch spec.DurationType {
	case config.CSAFE_DISTANCE_DURATION:
		done = phaseDistance >= spec.TargetDistance
	case config.CSAFE_CALORIES_DURATION:
		done = phaseCalories >= spec.TargetCalories
	default:
		done = phaseTime >= spec.TargetDuration
	}
	if done {
		ps.inRest = true
		fields.WorkoutState = config.WORKOUTSTATE_INTERVALREST
		fields.IntervalType = config.INTERVALTYPE_REST
		ps.phaseElapsed, ps.phaseDistance, ps.phaseCalories = elapsed, distance, calories
	}
	return fields, false
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
	s.finishWorkoutLocked()
}

// finishWorkoutLocked marks the current snapshot as a completed workout and
// moves the shared state machine to FINISHED. Shared by the flat-target
// (checkWorkoutComplete) and interval-schedule-exhausted (tick) paths. The
// caller must already hold s.mu -- both of those do, since they're called
// from within tick().
func (s *Simulator) finishWorkoutLocked() {
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

// finishWorkout is finishWorkoutLocked's counterpart for callers -- the CSV
// player -- that don't already hold s.mu.
func (s *Simulator) finishWorkout() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishWorkoutLocked()
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
