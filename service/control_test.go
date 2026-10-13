package service

import (
	"testing"
	"time"

	"pm5-emulator/config"
	"pm5-emulator/simulator"
	"pm5-emulator/sm"
)

func newTestMachineAndSim() (*sm.StateMachine, *simulator.Simulator) {
	machine := sm.NewStateMachine()
	machine.Reset()
	sim := simulator.NewSimulator(machine)
	return machine, sim
}

// waitForWorkoutType polls Snapshot() until WorkoutType is set (i.e. at
// least one Simulator tick has applied the just-configured workout),
// failing the test if it times out.
func waitForWorkoutType(t *testing.T, sim *simulator.Simulator, want byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sim.Snapshot().WorkoutType == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Snapshot().WorkoutType = %v, last seen %v", want, sim.Snapshot().WorkoutType)
}

// TestApplyPMConfig_FixedDistanceInterval decodes the "Fixed Distance Interval 500m/:30 rest"
// example frame verbatim from the bundled CSAFE spec (docs/resources/Concept2 PM CSAFE
// Communication Definition.pdf, "Sample Functionality"):
//
//	F1 76 15 01 01 07 03 05 80 00 00 01 F4 04 02 00 1E 14 01 01 13 02 01 01 0A F2
//
// data below is that frame's payload: everything between the 76/15 wrapper header and the
// trailing checksum/F2.
func TestApplyPMConfig_FixedDistanceInterval(t *testing.T) {
	configData := []byte{
		0x01, 0x01, 0x07, // SET_WORKOUTTYPE: WORKOUTTYPE_FIXEDDIST_INTERVAL
		0x03, 0x05, 0x80, 0x00, 0x00, 0x01, 0xF4, // SET_WORKOUTDURATION: distance, 500m
		0x04, 0x02, 0x00, 0x1E, // SET_RESTDURATION: 30s
		0x14, 0x01, 0x01, // CONFIGURE_WORKOUT: enable (commits the interval)
	}

	machine, sim := newTestMachineAndSim()
	applyPMConfig(machine, sim, configData)

	pmWorkoutMu.Lock()
	w := pmWorkout
	pmWorkoutMu.Unlock()

	if w.workoutType != config.WORKOUTTYPE_FIXEDDIST_INTERVAL {
		t.Fatalf("workoutType = %v, want FIXEDDIST_INTERVAL (%v)", w.workoutType, config.WORKOUTTYPE_FIXEDDIST_INTERVAL)
	}
	// A constant/single-interval workout's one goal lives in w.current, not w.intervals
	// (which commitInterval only populates for a variable workout's indexed schedule --
	// see TestApplyPMConfig_VariableInterval); startWorkout reads w.current directly.
	if len(w.intervals) != 0 {
		t.Fatalf("intervals = %+v, want none committed for a non-variable workout", w.intervals)
	}
	got := w.current
	if got.DurationType != config.CSAFE_DISTANCE_DURATION || got.TargetDistance != 500 {
		t.Errorf("interval = %+v, want DurationType=DISTANCE TargetDistance=500", got)
	}
	if got.RestDuration != 30*time.Second {
		t.Errorf("RestDuration = %v, want 30s", got.RestDuration)
	}
	if got.RestUndefined {
		t.Error("RestUndefined = true, want false (a plain FIXEDDIST_INTERVAL has a set rest length)")
	}

	// Feed the rest of the real frame (SET_SCREENSTATE/PREPARETOROWWORKOUT) to confirm it
	// actually starts the workout end-to-end.
	applyPMConfig(machine, sim, []byte{0x13, 0x02, 0x01, 0x01})

	if machine.GetStateName() != config.PM5_STATE_INUSE {
		t.Fatalf("state = %s, want INUSE after PREPARETOROWWORKOUT", machine.GetStateName())
	}

	go sim.Run()
	waitForWorkoutType(t, sim, config.WORKOUTTYPE_FIXEDDIST_INTERVAL)
	if ws := sim.Snapshot().WorkoutState; ws != config.WORKOUTSTATE_INTERVALWORKDISTANCE {
		t.Errorf("WorkoutState = %v, want INTERVALWORKDISTANCE", ws)
	}
}

// TestApplyPMConfig_VariableInterval decodes the "Variable Interval v500m/1:00r...4" example
// frame verbatim from the bundled CSAFE spec:
//
//	F1 76 6F 18 01 00 01 01 08 17 01 01 03 05 80 00 00 01 F4 04 02 00 3C 06 04 00 00 27 10 14 01 01 18 01 01 17 01
//	00 03 05 00 00 00 46 50 04 02 00 00 06 04 00 00 27 10 14 01 01 18 01 02 17 01 01 03 05 80 00 00 03 E8 04 02 00
//	00 06 04 00 00 27 10 14 01 01 18 01 03 17 01 00 03 05 00 00 00 75 30 04 02 00 78 06 04 00 00 27 10 14 01 01 13
//	02 01 01 C6 F2
//
// Four intervals: #1 500m/1:00r, #2 3:00/0:00r, #3 1000m/0:00r, #4 5:00/2:00r.
func TestApplyPMConfig_VariableInterval(t *testing.T) {
	configData := []byte{
		0x18, 0x01, 0x00, // SET_WORKOUTINTERVALCOUNT: interval #1 (idx 0)
		0x01, 0x01, 0x08, // SET_WORKOUTTYPE: WORKOUTTYPE_VARIABLE_INTERVAL
		0x17, 0x01, 0x01, // SET_INTERVALTYPE: DIST
		0x03, 0x05, 0x80, 0x00, 0x00, 0x01, 0xF4, // SET_WORKOUTDURATION: distance, 500m
		0x04, 0x02, 0x00, 0x3C, // SET_RESTDURATION: 60s
		0x06, 0x04, 0x00, 0x00, 0x27, 0x10, // SET_TARGETPACETIME (not modeled, ignored)
		0x14, 0x01, 0x01, // CONFIGURE_WORKOUT: commit interval 0

		0x18, 0x01, 0x01, // interval #2 (idx 1)
		0x17, 0x01, 0x00, // TIME
		0x03, 0x05, 0x00, 0x00, 0x00, 0x46, 0x50, // time, 3:00 (18000 centiseconds)
		0x04, 0x02, 0x00, 0x00, // rest = 0
		0x06, 0x04, 0x00, 0x00, 0x27, 0x10,
		0x14, 0x01, 0x01, // commit interval 1

		0x18, 0x01, 0x02, // interval #3 (idx 2)
		0x17, 0x01, 0x01, // DIST
		0x03, 0x05, 0x80, 0x00, 0x00, 0x03, 0xE8, // distance, 1000m
		0x04, 0x02, 0x00, 0x00, // rest = 0
		0x06, 0x04, 0x00, 0x00, 0x27, 0x10,
		0x14, 0x01, 0x01, // commit interval 2

		0x18, 0x01, 0x03, // interval #4 (idx 3)
		0x17, 0x01, 0x00, // TIME
		0x03, 0x05, 0x00, 0x00, 0x00, 0x75, 0x30, // time, 5:00 (30000 centiseconds)
		0x04, 0x02, 0x00, 0x78, // rest = 2:00 (120s)
		0x06, 0x04, 0x00, 0x00, 0x27, 0x10,
		0x14, 0x01, 0x01, // commit interval 3
	}

	machine, sim := newTestMachineAndSim()
	applyPMConfig(machine, sim, configData)

	pmWorkoutMu.Lock()
	w := pmWorkout
	pmWorkoutMu.Unlock()

	if w.workoutType != config.WORKOUTTYPE_VARIABLE_INTERVAL {
		t.Fatalf("workoutType = %v, want VARIABLE_INTERVAL (%v)", w.workoutType, config.WORKOUTTYPE_VARIABLE_INTERVAL)
	}
	if len(w.intervals) != 4 {
		t.Fatalf("intervals = %+v, want exactly 4", w.intervals)
	}

	want := []simulator.IntervalSpec{
		{DurationType: config.CSAFE_DISTANCE_DURATION, TargetDistance: 500, RestDuration: 60 * time.Second},
		{DurationType: config.CSAFE_TIME_DURATION, TargetDuration: 180 * time.Second, RestDuration: 0},
		{DurationType: config.CSAFE_DISTANCE_DURATION, TargetDistance: 1000, RestDuration: 0},
		{DurationType: config.CSAFE_TIME_DURATION, TargetDuration: 300 * time.Second, RestDuration: 120 * time.Second},
	}
	for i, spec := range want {
		got := w.intervals[i]
		if got.DurationType != spec.DurationType || got.TargetDistance != spec.TargetDistance ||
			got.TargetDuration != spec.TargetDuration || got.RestDuration != spec.RestDuration {
			t.Errorf("interval[%d] = %+v, want %+v", i, got, spec)
		}
	}

	// Feed the final SET_SCREENSTATE/PREPARETOROWWORKOUT to start it end-to-end.
	applyPMConfig(machine, sim, []byte{0x13, 0x02, 0x01, 0x01})

	if machine.GetStateName() != config.PM5_STATE_INUSE {
		t.Fatalf("state = %s, want INUSE after PREPARETOROWWORKOUT", machine.GetStateName())
	}

	go sim.Run()
	waitForWorkoutType(t, sim, config.WORKOUTTYPE_VARIABLE_INTERVAL)
	// The first configured interval is distance-based (500m).
	if ws := sim.Snapshot().WorkoutState; ws != config.WORKOUTSTATE_INTERVALWORKDISTANCE {
		t.Errorf("WorkoutState = %v, want INTERVALWORKDISTANCE (interval #1 is distance-based)", ws)
	}
}

// The spec's "Variable Interval Undefined Rest" example (pages 96-97) of
// docs/resources/Concept2 PM CSAFE Communication Definition.pdf programs the intervals as variable
// intervals with "rest undefined" interval types and only then changes the workout type to
// undefined-rest. That final type change must keep the intervals already configured.
//
//	Interval 1: 100m, interval 2: 2:00, both with undefined rest.
func TestApplyPMConfig_VariableIntervalUndefinedRest(t *testing.T) {
	configData := []byte{
		0x18, 0x01, 0x00, // interval #1 (idx 0)
		0x01, 0x01, 0x08, // SET_WORKOUTTYPE: WORKOUTTYPE_VARIABLE_INTERVAL
		0x17, 0x01, 0x04, // SET_INTERVALTYPE: DISTANCERESTUNDEFINED
		0x03, 0x05, 0x80, 0x00, 0x00, 0x00, 0x64, // distance, 100m
		0x04, 0x02, 0x00, 0x00, // rest = 0
		0x06, 0x04, 0x00, 0x00, 0x32, 0xC8, // SET_TARGETPACETIME (ignored)
		0x14, 0x01, 0x01, // commit interval 0

		0x18, 0x01, 0x01, // interval #2 (idx 1)
		0x17, 0x01, 0x03, // TIMERESTUNDEFINED
		0x03, 0x05, 0x00, 0x00, 0x00, 0x2E, 0xE0, // time, 2:00 (12000 centiseconds)
		0x04, 0x02, 0x00, 0x00,
		0x06, 0x04, 0x00, 0x00, 0x32, 0xC8,
		0x14, 0x01, 0x01, // commit interval 1

		0x01, 0x01, 0x09, // SET_WORKOUTTYPE: WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL
		0x05, 0x05, 0x80, 0x00, 0x00, 0x00, 0x00, // SET_SPLITDURATION: 0m
	}

	machine, sim := newTestMachineAndSim()
	applyPMConfig(machine, sim, configData)

	pmWorkoutMu.Lock()
	w := pmWorkout
	pmWorkoutMu.Unlock()

	if w.workoutType != config.WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL {
		t.Fatalf("workoutType = %v, want VARIABLE_UNDEFINEDREST_INTERVAL", w.workoutType)
	}
	if len(w.intervals) != 2 {
		t.Fatalf("intervals = %+v, want exactly 2", w.intervals)
	}
	if got := w.intervals[0]; got.DurationType != config.CSAFE_DISTANCE_DURATION || got.TargetDistance != 100 || !got.RestUndefined {
		t.Errorf("interval[0] = %+v, want 100m with undefined rest", got)
	}
	if got := w.intervals[1]; got.DurationType != config.CSAFE_TIME_DURATION || got.TargetDuration != 120*time.Second || !got.RestUndefined {
		t.Errorf("interval[1] = %+v, want 2:00 with undefined rest", got)
	}

	applyPMConfig(machine, sim, []byte{0x13, 0x02, 0x01, 0x01})
	if machine.GetStateName() != config.PM5_STATE_INUSE {
		t.Fatalf("state = %s, want INUSE after PREPARETOROWWORKOUT", machine.GetStateName())
	}
}

// A mix of timed and undefined rests is programmed the same way; only the intervals whose type is
// a "rest undefined" variant are undefined.
func TestApplyPMConfig_VariableIntervalMixedRest(t *testing.T) {
	configData := []byte{
		0x18, 0x01, 0x00,
		0x01, 0x01, 0x08,
		0x17, 0x01, 0x01, // DIST, with a rest
		0x03, 0x05, 0x80, 0x00, 0x00, 0x01, 0xF4,
		0x04, 0x02, 0x00, 0x3C,
		0x14, 0x01, 0x01,

		0x18, 0x01, 0x01,
		0x17, 0x01, 0x03, // TIMERESTUNDEFINED
		0x03, 0x05, 0x00, 0x00, 0x00, 0x2E, 0xE0,
		0x04, 0x02, 0x00, 0x00,
		0x14, 0x01, 0x01,

		0x01, 0x01, 0x09,
	}

	machine, sim := newTestMachineAndSim()
	applyPMConfig(machine, sim, configData)

	pmWorkoutMu.Lock()
	w := pmWorkout
	pmWorkoutMu.Unlock()

	if len(w.intervals) != 2 {
		t.Fatalf("intervals = %+v, want exactly 2", w.intervals)
	}
	if w.intervals[0].RestUndefined || w.intervals[0].RestDuration != 60*time.Second {
		t.Errorf("interval[0] = %+v, want a 60s rest", w.intervals[0])
	}
	if !w.intervals[1].RestUndefined {
		t.Errorf("interval[1] = %+v, want an undefined rest", w.intervals[1])
	}
}
