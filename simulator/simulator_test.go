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
