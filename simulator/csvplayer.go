package simulator

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"pm5-emulator/config"
)

// csvTimeLayout matches the wall-clock timestamps a recorded PM5 session
// uses, e.g. "2026-09-25T14:36:36.165154".
const csvTimeLayout = "2006-01-02T15:04:05.999999"

// csvFrame is one row of a recorded session: the cumulative LiveState after
// applying that row's field update (or, for a force_curve.points row, the
// unchanged state plus the completed stroke's raw points).
type csvFrame struct {
	at         time.Time
	state      LiveState
	forceCurve []uint16
}

// CSVTimeline is a parsed, ready-to-replay recorded PM5 session.
type CSVTimeline struct {
	frames   []csvFrame
	startIdx int
	endIdx   int // inclusive
}

func secondsToDuration(v float64) time.Duration {
	return time.Duration(v * float64(time.Second))
}

// fieldSetters maps every "key" this CSV format is known to produce (a
// dotted characteristic-group + field name, e.g. "general.rowing_state",
// "stroke.drive_force.max") to a function applying its numeric value to a
// LiveState. Every value in this format is a plain number (or blank), so
// all of them are parsed once as float64 regardless of the field's final
// type/units.
var fieldSetters = map[string]func(s *LiveState, v float64){
	"general.elapsed_time":        func(s *LiveState, v float64) { s.ElapsedTime = secondsToDuration(v) },
	"general.distance":            func(s *LiveState, v float64) { s.Distance = v },
	"general.workout_type":        func(s *LiveState, v float64) { s.WorkoutType = byte(v) },
	"general.interval_type":       func(s *LiveState, v float64) { s.IntervalType = byte(v) },
	"general.workout_state":       func(s *LiveState, v float64) { s.WorkoutState = byte(v) },
	"general.rowing_state":        func(s *LiveState, v float64) { s.RowingState = byte(v) },
	"general.stroke_state":        func(s *LiveState, v float64) { s.StrokeState = byte(v) },
	"general.total_work_distance": func(s *LiveState, v float64) { s.TotalWorkDistance = v },
	"general.workout_duration":    func(s *LiveState, v float64) { s.WorkoutDuration = secondsToDuration(v) },
	"general.drag_factor":         func(s *LiveState, v float64) { s.DragFactor = byte(v) },

	"status1.elapsed_time":  func(s *LiveState, v float64) { s.ElapsedTime = secondsToDuration(v) },
	"status1.speed":         func(s *LiveState, v float64) { s.Speed = v },
	"status1.stroke_rate":   func(s *LiveState, v float64) { s.StrokeRate = byte(v) },
	"status1.heart_rate":    func(s *LiveState, v float64) { s.HeartRate = byte(v) },
	"status1.current_pace":  func(s *LiveState, v float64) { s.CurrentPace = secondsToDuration(v) },
	"status1.average_pace":  func(s *LiveState, v float64) { s.AvgPace = secondsToDuration(v) },
	"status1.rest_distance": func(s *LiveState, v float64) { s.RestDistance = v },
	"status1.rest_time":     func(s *LiveState, v float64) { s.RestTime = secondsToDuration(v) },

	"status2.elapsed_time":           func(s *LiveState, v float64) { s.ElapsedTime = secondsToDuration(v) },
	"status2.interval_count":         func(s *LiveState, v float64) { s.IntervalCount = byte(v) },
	"status2.average_power":          func(s *LiveState, v float64) { s.AveragePower = v },
	"status2.total_calories":         func(s *LiveState, v float64) { s.TotalCalories = v },
	"status2.split_average_pace":     func(s *LiveState, v float64) { s.SplitAvgPace = secondsToDuration(v) },
	"status2.split_average_power":    func(s *LiveState, v float64) { s.SplitAvgPower = v },
	"status2.split_average_calories": func(s *LiveState, v float64) { s.SplitAvgCalories = v },
	"status2.last_split_time":        func(s *LiveState, v float64) { s.LastSplitTime = secondsToDuration(v) },
	"status2.last_split_distance":    func(s *LiveState, v float64) { s.LastSplitDistance = v },

	"status3.operational_state":  func(s *LiveState, v float64) { s.OperationalState = byte(v) },
	"status3.verification_state": func(s *LiveState, v float64) { s.VerificationState = byte(v) },
	"status3.screen_number":      func(s *LiveState, v float64) { s.ScreenNumber = uint16(v) },
	"status3.last_error":         func(s *LiveState, v float64) { s.LastError = uint16(v) },
	"status3.game_id":            func(s *LiveState, v float64) { s.GameID = byte(v) },
	"status3.game_score":         func(s *LiveState, v float64) { s.GameScore = uint16(v) },
	"status3.battery_level":      func(s *LiveState, v float64) { s.BatteryLevel = byte(v) },

	"stroke.elapsed_time":            func(s *LiveState, v float64) { s.ElapsedTime = secondsToDuration(v) },
	"stroke.distance":                func(s *LiveState, v float64) { s.Distance = v },
	"stroke.drive_length":            func(s *LiveState, v float64) { s.DriveLength = v },
	"stroke.drive_time":              func(s *LiveState, v float64) { s.DriveTime = secondsToDuration(v) },
	"stroke.recovery_time":           func(s *LiveState, v float64) { s.StrokeRecoveryTime = secondsToDuration(v) },
	"stroke.distance_per_stroke":     func(s *LiveState, v float64) { s.StrokeDistance = v },
	"stroke.drive_force.max":         func(s *LiveState, v float64) { s.PeakDriveForce = v },
	"stroke.drive_force.average":     func(s *LiveState, v float64) { s.AvgDriveForce = v },
	"stroke.work_per_stroke":         func(s *LiveState, v float64) { s.WorkPerStroke = v },
	"stroke.count":                   func(s *LiveState, v float64) { s.StrokeCount = uint16(v) },
	"stroke.power":                   func(s *LiveState, v float64) { s.StrokePower = v },
	"stroke.calories":                func(s *LiveState, v float64) { s.StrokeCalories = v },
	"stroke.projected_work_time":     func(s *LiveState, v float64) { s.ProjectedWorkTime = secondsToDuration(v) },
	"stroke.projected_work_distance": func(s *LiveState, v float64) { s.ProjectedWorkDistance = v },
}

// LoadCSVTimeline parses a recorded PM5 session -- a "timestamp,key,value"
// CSV, where key is a dotted characteristic-group + field name and value is
// that field's new reading at that instant (numeric, blank, or, for
// "force_curve.points", a ';'-separated list of one completed stroke's
// force-curve points) -- into a ready-to-replay CSVTimeline. Any
// unrecognized key or unparseable value is a hard error: better to fail
// fast at startup than silently drop data from a file meant to be replayed
// in full.
func LoadCSVTimeline(path string) (*CSVTimeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = 3

	if _, err := r.Read(); err != nil { // header
		return nil, fmt.Errorf("reading header: %w", err)
	}

	current := newLiveState()
	var frames []csvFrame
	lineNum := 1

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		lineNum++

		ts, err := time.Parse(csvTimeLayout, record[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: bad timestamp %q: %w", lineNum, record[0], err)
		}
		key, rawValue := record[1], record[2]

		if key == "force_curve.points" {
			points, err := parseForceCurvePoints(rawValue)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNum, err)
			}
			frames = append(frames, csvFrame{at: ts, state: current, forceCurve: points})
			continue
		}

		setter, ok := fieldSetters[key]
		if !ok {
			return nil, fmt.Errorf("line %d: unrecognized key %q", lineNum, key)
		}
		if strings.TrimSpace(rawValue) != "" {
			v, err := strconv.ParseFloat(rawValue, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad value %q for key %q: %w", lineNum, rawValue, key, err)
			}
			setter(&current, v)
		}
		frames = append(frames, csvFrame{at: ts, state: current})
	}

	if len(frames) == 0 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}

	t := &CSVTimeline{frames: frames}
	if err := t.computeBounds(); err != nil {
		return nil, err
	}
	return t, nil
}

func parseForceCurvePoints(raw string) ([]uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ";")
	points := make([]uint16, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("bad force curve point %q: %w", p, err)
		}
		points = append(points, uint16(v))
	}
	return points, nil
}

// computeBounds finds where playback should start (the first drive of the
// first stroke) and end (the last frame of the last *confirmed-complete*
// stroke -- a stroke is only confirmed complete once another one begins
// after it, so the final, possibly-incomplete stroke is always cut).
func (t *CSVTimeline) computeBounds() error {
	startIdx := -1
	for i, f := range t.frames {
		if f.state.RowingState == config.ROWINGSTATE_ACTIVE && f.state.StrokeState == config.STROKESTATE_DRIVING_STATE {
			startIdx = i
			break
		}
	}
	if startIdx == -1 {
		return fmt.Errorf("no frame found with RowingState=ACTIVE and StrokeState=DRIVING; cannot determine playback start")
	}

	var boundaries []int
	for i := startIdx + 1; i < len(t.frames); i++ {
		if t.frames[i].state.StrokeCount != t.frames[i-1].state.StrokeCount {
			boundaries = append(boundaries, i)
		}
	}

	endIdx := len(t.frames) - 1
	if len(boundaries) >= 2 {
		endIdx = boundaries[len(boundaries)-1] - 1
	}

	t.startIdx = startIdx
	t.endIdx = endIdx
	return nil
}

// Play advances the timeline forever, replaying frames[startIdx..endIdx] in
// a loop whenever the shared state machine reports INUSE, and idling
// otherwise. A paused/reset/reconnected session always resumes from the
// first stroke (startIdx) once INUSE again, not wherever it left off.
func (t *CSVTimeline) Play(sim *Simulator) {
	for {
		if sim.sm.GetState() != sim.sm.INUSE {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.playOnce(sim)
	}
}

func (t *CSVTimeline) playOnce(sim *Simulator) {
	base := t.frames[t.startIdx].at
	realStart := time.Now()
	restart := sim.restartSignal()

	for i := t.startIdx; i <= t.endIdx; i++ {
		if sim.sm.GetState() != sim.sm.INUSE {
			return
		}
		select {
		case <-restart:
			return
		default:
		}

		f := t.frames[i]
		if wait := f.at.Sub(base) - time.Since(realStart); wait > 0 {
			select {
			case <-restart:
				return
			case <-time.After(wait):
			}
		}

		sim.applyFrame(f.state)
		if f.forceCurve != nil {
			sim.publishForceCurve(f.forceCurve)
		}
	}
}
