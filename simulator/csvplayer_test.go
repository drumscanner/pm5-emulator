package simulator

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"pm5-emulator/config"
	"pm5-emulator/sm"
)

// writeCSV creates a temp CSV file with the given raw body (header is added
// automatically) and returns its path.
func writeCSV(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.csv")
	content := "timestamp,key,value\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test CSV: %v", err)
	}
	return path
}

func TestLoadCSVTimeline_BasicFieldMapping(t *testing.T) {
	path := writeCSV(t, ""+
		"2026-01-01T00:00:00.000000,general.rowing_state,0\n"+
		"2026-01-01T00:00:00.100000,general.rowing_state,1\n"+
		"2026-01-01T00:00:00.100000,general.stroke_state,2\n"+
		"2026-01-01T00:00:00.200000,general.distance,10.5\n"+
		"2026-01-01T00:00:00.300000,stroke.count,1\n"+
		"2026-01-01T00:00:00.400000,stroke.count,2\n",
	)

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}

	// Start should be the frame where rowing_state=ACTIVE and stroke_state=DRIVING
	// both hold, i.e. the row that set stroke_state to DRIVING (index 2).
	startFrame := timeline.frames[timeline.startIdx]
	if startFrame.state.RowingState != config.ROWINGSTATE_ACTIVE || startFrame.state.StrokeState != config.STROKESTATE_DRIVING_STATE {
		t.Fatalf("startIdx frame = %+v, want RowingState=ACTIVE StrokeState=DRIVING", startFrame.state)
	}

	last := timeline.frames[len(timeline.frames)-1]
	if last.state.Distance != 10.5 {
		t.Errorf("Distance = %v, want 10.5", last.state.Distance)
	}
	if last.state.StrokeCount != 2 {
		t.Errorf("StrokeCount = %v, want 2", last.state.StrokeCount)
	}
}

func TestLoadCSVTimeline_BlankValueIsNoOp(t *testing.T) {
	path := writeCSV(t, ""+
		"2026-01-01T00:00:00.000000,general.rowing_state,1\n"+
		"2026-01-01T00:00:00.000000,general.stroke_state,2\n"+
		"2026-01-01T00:00:00.100000,general.workout_duration,\n",
	)

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}
	last := timeline.frames[len(timeline.frames)-1]
	if last.state.WorkoutDuration != 0 {
		t.Errorf("WorkoutDuration = %v, want 0 (blank value)", last.state.WorkoutDuration)
	}
}

func TestLoadCSVTimeline_UnrecognizedKeyErrors(t *testing.T) {
	path := writeCSV(t, "2026-01-01T00:00:00.000000,general.made_up_field,1\n")
	if _, err := LoadCSVTimeline(path); err == nil {
		t.Fatal("expected an error for an unrecognized key, got nil")
	}
}

func TestLoadCSVTimeline_ForceCurveEvent(t *testing.T) {
	path := writeCSV(t, ""+
		"2026-01-01T00:00:00.000000,general.rowing_state,1\n"+
		"2026-01-01T00:00:00.000000,general.stroke_state,2\n"+
		"2026-01-01T00:00:00.100000,force_curve.points,1;2;300;65535\n",
	)

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}

	last := timeline.frames[len(timeline.frames)-1]
	want := []uint16{1, 2, 300, 65535}
	if len(last.forceCurve) != len(want) {
		t.Fatalf("forceCurve = %v, want %v", last.forceCurve, want)
	}
	for i := range want {
		if last.forceCurve[i] != want[i] {
			t.Errorf("forceCurve[%d] = %v, want %v", i, last.forceCurve[i], want[i])
		}
	}
}

// buildStrokeRecording produces a recording with numStrokes strokes, each
// consisting of a "drive start" frame (rowing_state=ACTIVE,
// stroke_state=DRIVING, stroke_count=N) 100ms after the previous one ends,
// for use in trimming tests.
func buildStrokeRecording(numStrokes int) string {
	body := ""
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= numStrokes; i++ {
		ts := at.Format(csvTimeLayout)
		body += ts + ",general.rowing_state,1\n"
		body += ts + ",general.stroke_state,2\n"
		body += ts + ",stroke.count," + strconv.Itoa(i) + "\n"
		at = at.Add(100 * time.Millisecond)
	}
	return body
}

func TestLoadCSVTimeline_TrimsIncompleteLastStroke(t *testing.T) {
	// 4 strokes recorded; the 4th (last) one must be cut, since we can never
	// confirm it completed -- only that a 5th one began, which it didn't.
	path := writeCSV(t, buildStrokeRecording(4))

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}

	endFrame := timeline.frames[timeline.endIdx]
	if endFrame.state.StrokeCount != 3 {
		t.Errorf("last playable StrokeCount = %v, want 3 (stroke 4 should be cut)", endFrame.state.StrokeCount)
	}
	// Nothing past endIdx should be reachable from a stroke-count perspective
	// during playback, i.e. endIdx must be strictly before the frame where
	// StrokeCount becomes 4.
	for i := timeline.startIdx; i <= timeline.endIdx; i++ {
		if timeline.frames[i].state.StrokeCount == 4 {
			t.Fatalf("frame %d has StrokeCount=4 within [startIdx,endIdx], should have been trimmed", i)
		}
	}
}

func TestLoadCSVTimeline_TooFewStrokesFallsBackToWholeFile(t *testing.T) {
	// Only 1 stroke ever recorded: nothing to confirm as "complete", so
	// there's no safe trim point -- play the whole (short) recording.
	path := writeCSV(t, buildStrokeRecording(1))

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}
	if timeline.endIdx != len(timeline.frames)-1 {
		t.Errorf("endIdx = %d, want %d (whole file)", timeline.endIdx, len(timeline.frames)-1)
	}
}

func TestLoadCSVTimeline_NoActiveDriveFrameErrors(t *testing.T) {
	path := writeCSV(t, "2026-01-01T00:00:00.000000,general.rowing_state,0\n")
	if _, err := LoadCSVTimeline(path); err == nil {
		t.Fatal("expected an error when no ACTIVE+DRIVING frame exists")
	}
}

func TestCSVTimeline_Play_RespectsStateMachineAndLoops(t *testing.T) {
	// Two strokes recorded 50ms apart (real time), then a trailing partial
	// stroke that must be cut; timing is fast enough to observe a couple of
	// loop iterations within the test timeout.
	body := ""
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		ts := at.Format(csvTimeLayout)
		body += ts + ",general.rowing_state,1\n"
		body += ts + ",general.stroke_state,2\n"
		body += ts + ",general.distance," + strconv.Itoa(i*10) + "\n"
		body += ts + ",stroke.count," + strconv.Itoa(i) + "\n"
		at = at.Add(20 * time.Millisecond)
	}
	path := writeCSV(t, body)

	timeline, err := LoadCSVTimeline(path)
	if err != nil {
		t.Fatalf("LoadCSVTimeline: %v", err)
	}

	machine := sm.NewStateMachine()
	machine.Reset()
	_ = machine.Update(config.CSAFE_GOIDLE_CMD)
	_ = machine.Update(config.CSAFE_GOHAVEID_CMD)
	_ = machine.Update(config.CSAFE_GOINUSE_CMD)

	sim := NewSimulator(machine)
	done := make(chan struct{})
	go func() {
		timeline.Play(sim)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	sawDistance20 := false
	for time.Now().Before(deadline) {
		if sim.Snapshot().Distance == 20 {
			sawDistance20 = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawDistance20 {
		t.Fatal("timed out waiting for playback to reach the 2nd (last confirmed-complete) stroke's distance")
	}

	select {
	case <-done:
		t.Fatal("Play returned, but it should loop forever")
	default:
	}
}
