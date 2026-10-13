package simulator

import (
	"testing"
	"time"

	"pm5-emulator/config"
	"pm5-emulator/sm"
)

func newInUseSimulator() *Simulator {
	machine := sm.NewStateMachine()
	machine.Reset()
	_ = machine.Update(config.CSAFE_GOIDLE_CMD)
	_ = machine.Update(config.CSAFE_GOHAVEID_CMD)
	_ = machine.Update(config.CSAFE_GOINUSE_CMD)

	return NewSimulator(machine)
}

func TestPaceToWatts(t *testing.T) {
	// 2:00/500m is a well known reference: ~exercise ~ 2.80/(0.24)^3 = ~202W
	got := paceToWatts(120)
	if got < 195 || got > 210 {
		t.Errorf("paceToWatts(120) = %v, want ~202", got)
	}

	// Faster pace (lower split time) must produce more power.
	if paceToWatts(100) <= paceToWatts(120) {
		t.Errorf("expected faster pace to produce more power")
	}
}

func TestTick_AdvancesDistanceAndTime(t *testing.T) {
	s := newInUseSimulator()
	s.targetSplitSec = 120 // 2:00/500m => speed = 500/120 m/s

	for i := 0; i < 10; i++ {
		s.tick(1 * time.Second)
	}

	got := s.Snapshot()
	if got.ElapsedTime != 10*time.Second {
		t.Errorf("ElapsedTime = %v, want 10s", got.ElapsedTime)
	}

	wantDistance := 500.0 / 120.0 * 10
	if diff := got.Distance - wantDistance; diff < -1 || diff > 1 {
		t.Errorf("Distance = %v, want ~%v", got.Distance, wantDistance)
	}

	if got.TotalCalories <= 0 {
		t.Errorf("expected calories to accumulate, got %v", got.TotalCalories)
	}
}

func TestTick_NoOpWhenNotInUse(t *testing.T) {
	machine := sm.NewStateMachine()
	machine.Reset() // READY, not INUSE
	s := NewSimulator(machine)

	s.tick(5 * time.Second)

	got := s.Snapshot()
	if got.ElapsedTime != 0 || got.Distance != 0 {
		t.Errorf("expected no progress while not INUSE, got elapsed=%v distance=%v", got.ElapsedTime, got.Distance)
	}
}

func TestUpdateSplit_FiresAt500Meters(t *testing.T) {
	s := newInUseSimulator()
	s.targetSplitSec = 100 // fast pace so the 500m boundary is reached quickly
	splitEvents, _, unsubscribe := s.Subscribe()
	defer unsubscribe()

	splitFired := false
	for i := 0; i < 200; i++ {
		s.tick(1 * time.Second)
		select {
		case <-splitEvents:
			splitFired = true
		default:
		}
		if splitFired {
			break
		}
	}

	if !splitFired {
		t.Fatal("expected a split event after crossing 500m")
	}

	got := s.Snapshot()
	if got.SplitIntervalNumber != 1 {
		t.Errorf("SplitIntervalNumber = %v, want 1", got.SplitIntervalNumber)
	}
	if got.LastSplitDistance < splitDistance {
		t.Errorf("LastSplitDistance = %v, want >= %v", got.LastSplitDistance, splitDistance)
	}
}

func TestCheckWorkoutComplete_FixedDistance(t *testing.T) {
	s := newInUseSimulator()
	s.targetSplitSec = 100
	s.ConfigureWorkout(WorkoutConfig{
		WorkoutType:    config.WORKOUTTYPE_FIXEDDIST_NOSPLITS,
		DurationType:   config.CSAFE_DISTANCE_DURATION,
		TargetDistance: 200,
		TargetPaceSec:  100,
	})
	_, summaryEvents, unsubscribe := s.Subscribe()
	defer unsubscribe()

	summaryFired := false
	for i := 0; i < 200; i++ {
		s.tick(1 * time.Second)
		select {
		case <-summaryEvents:
			summaryFired = true
		default:
		}
		if summaryFired {
			break
		}
	}

	if !summaryFired {
		t.Fatal("expected a summary event once the target distance was reached")
	}
	if !s.sm.IsFinished() {
		t.Errorf("expected state machine to be FINISHED, got %s", s.sm.GetStateName())
	}

	// Further ticks must not advance distance/time once finished.
	before := s.Snapshot()
	s.tick(5 * time.Second)
	after := s.Snapshot()
	if after.ElapsedTime != before.ElapsedTime || after.Distance != before.Distance {
		t.Errorf("expected simulation to freeze after finishing, got before=%+v after=%+v", before, after)
	}
}

func TestAdvanceIntervalPhase_WorkRestWorkRepeat(t *testing.T) {
	schedule := []IntervalSpec{{
		DurationType:   config.CSAFE_TIME_DURATION,
		TargetDuration: 5 * time.Second,
		RestDuration:   2 * time.Second,
	}}
	var phase intervalPhase
	idx := 0
	var elapsed time.Duration

	step := func(dt time.Duration) intervalFields {
		elapsed += dt
		fields, done := advanceIntervalPhase(&phase, schedule, &idx, true, elapsed, 0, 0)
		if done {
			t.Fatalf("schedule should never be done when repeat=true (elapsed=%v)", elapsed)
		}
		return fields
	}

	var fields intervalFields
	for i := 0; i < 4; i++ {
		fields = step(time.Second) // elapsed 1,2,3,4s: still working
	}
	if fields.WorkoutState != config.WORKOUTSTATE_INTERVALWORKTIME || fields.IntervalCount != 1 {
		t.Fatalf("after 4s: WorkoutState=%v IntervalCount=%v, want WORKTIME/1", fields.WorkoutState, fields.IntervalCount)
	}

	fields = step(time.Second) // elapsed 5s: work target reached, flips to rest immediately
	if fields.WorkoutState != config.WORKOUTSTATE_INTERVALREST || fields.IntervalCount != 1 {
		t.Fatalf("after 5s: WorkoutState=%v IntervalCount=%v, want REST/1", fields.WorkoutState, fields.IntervalCount)
	}

	fields = step(time.Second) // elapsed 6s: 1s into the 2s rest
	if fields.WorkoutState != config.WORKOUTSTATE_INTERVALREST || fields.RestTime != time.Second {
		t.Fatalf("after 6s: WorkoutState=%v RestTime=%v, want REST/1s", fields.WorkoutState, fields.RestTime)
	}

	fields = step(time.Second) // elapsed 7s: rest target reached (2s rest complete)
	if fields.WorkoutState != config.WORKOUTSTATE_INTERVALREST {
		t.Fatalf("after 7s: WorkoutState=%v, want REST (transition observed on the next call)", fields.WorkoutState)
	}

	fields = step(time.Second) // elapsed 8s: back in work, 2nd interval
	if fields.WorkoutState != config.WORKOUTSTATE_INTERVALWORKTIME || fields.IntervalCount != 2 {
		t.Fatalf("after 8s: WorkoutState=%v IntervalCount=%v, want WORKTIME/2", fields.WorkoutState, fields.IntervalCount)
	}
}

func TestAdvanceIntervalPhase_FiniteScheduleExhausts(t *testing.T) {
	schedule := []IntervalSpec{
		{DurationType: config.CSAFE_DISTANCE_DURATION, TargetDistance: 100, RestDuration: 0},
		{DurationType: config.CSAFE_DISTANCE_DURATION, TargetDistance: 200, RestDuration: 0},
	}
	var phase intervalPhase
	idx := 0

	// Drive distance up in small steps (elapsed ticks alongside, unused by a
	// distance-typed interval's own completion check) until the schedule
	// reports done; it must not do so before both intervals' distance is
	// covered, and must do so once it is.
	var elapsed time.Duration
	var distance float64
	var lastDone bool
	for step := 0; step < 1000 && !lastDone; step++ {
		elapsed += 100 * time.Millisecond
		distance += 1
		_, done := advanceIntervalPhase(&phase, schedule, &idx, false, elapsed, distance, 0)
		lastDone = done
		if done {
			break
		}
		if distance < 100+200 && idx >= len(schedule) {
			t.Fatalf("scheduleIdx ran past the end before covering both intervals' distance (distance=%v)", distance)
		}
	}
	if !lastDone {
		t.Fatal("expected the finite schedule to report done once both intervals (and the 2nd's rest, 0s) completed")
	}
}

func TestSimulator_Tick_IntervalSchedule(t *testing.T) {
	s := newInUseSimulator()
	s.targetSplitSec = 100 // keep speed/power > 0 so distance/calories actually move
	s.ConfigureWorkout(WorkoutConfig{
		WorkoutType: config.WORKOUTTYPE_FIXEDTIME_INTERVAL,
		Intervals: []IntervalSpec{
			{DurationType: config.CSAFE_TIME_DURATION, TargetDuration: 2 * time.Second, RestDuration: 1 * time.Second},
		},
		Repeat: true,
	})

	var sawRest, sawWorkAfterRest bool
	var lastDistance float64
	for i := 0; i < 10; i++ {
		s.tick(1 * time.Second)
		got := s.Snapshot()

		// Distance/calories must keep accumulating through rest too (the
		// confirmed, shipped behavior -- "the rower keeps rowing through
		// the rest"), never freeze or go backwards.
		if got.Distance < lastDistance {
			t.Fatalf("tick %d: Distance went backwards: %v -> %v", i, lastDistance, got.Distance)
		}
		lastDistance = got.Distance

		if got.WorkoutState == config.WORKOUTSTATE_INTERVALREST {
			sawRest = true
		} else if got.WorkoutState == config.WORKOUTSTATE_INTERVALWORKTIME && sawRest {
			sawWorkAfterRest = true
		}
	}

	if !sawRest {
		t.Error("expected WorkoutState to show INTERVALREST at some point")
	}
	if !sawWorkAfterRest {
		t.Error("expected WorkoutState to return to INTERVALWORKTIME after a rest (repeat=true)")
	}
	if lastDistance <= 0 {
		t.Error("expected Distance to have advanced over 10 ticks")
	}
}

func TestSimulator_Tick_IntervalScheduleFinishes(t *testing.T) {
	s := newInUseSimulator()
	s.targetSplitSec = 100
	s.ConfigureWorkout(WorkoutConfig{
		WorkoutType: config.WORKOUTTYPE_FIXEDTIME_INTERVAL,
		Intervals: []IntervalSpec{
			{DurationType: config.CSAFE_TIME_DURATION, TargetDuration: 1 * time.Second, RestDuration: 0},
		},
		Repeat: false, // a single, finite interval: must finish, not repeat forever
	})

	for i := 0; i < 20 && !s.sm.IsFinished(); i++ {
		s.tick(500 * time.Millisecond)
	}

	if !s.sm.IsFinished() {
		t.Fatal("expected the state machine to reach FINISHED once the single interval completed")
	}
	if got := s.Snapshot().WorkoutState; got != config.WORKOUTSTATE_WORKOUTEND {
		t.Errorf("WorkoutState = %v, want WORKOUTSTATE_WORKOUTEND", got)
	}
}

func TestReset_ZeroesLiveCounters(t *testing.T) {
	s := newInUseSimulator()
	s.tick(5 * time.Second)

	s.Reset()

	got := s.Snapshot()
	if got.ElapsedTime != 0 || got.Distance != 0 || got.StrokeCount != 0 {
		t.Errorf("expected zeroed state after Reset, got %+v", got)
	}
	if got.HeartRate != 255 {
		t.Errorf("HeartRate = %v, want 255 (invalid/no belt)", got.HeartRate)
	}
}
