package service

import (
	"sync"
	"sync/atomic"
	"time"

	"pm5-emulator/config"
	"pm5-emulator/protocol/csafe"
	"pm5-emulator/simulator"
	"pm5-emulator/sm"

	"github.com/bettercap/gatt"
	"github.com/sirupsen/logrus"
)

/*
C2 PM control
primary service
*/
var (
	attrControlServiceUUID, _          = gatt.ParseUUID(getFullUUID("0020"))
	attrReceiveCharacteristicsUUID, _  = gatt.ParseUUID(getFullUUID("0021"))
	attrTransmitCharacteristicsUUID, _ = gatt.ParseUUID(getFullUUID("0022"))
)

// responsePollInterval bounds how long the transmit notify loop blocks
// between checking whether the client has unsubscribed (gatt.Notifier has no
// blocking/cancelable wait, only a Done() poll).
const responsePollInterval = 200 * time.Millisecond

// NewControlService advertises the Control service offered by PM5. Incoming
// CSAFE commands on the receive characteristic (0x0021) drive the shared
// state machine and the workout simulator's configuration; a CSAFE response
// frame is pushed back on the transmit characteristic (0x0022) for each
// command processed, the way real PM5 clients (e.g. ErgData) expect.
func NewControlService(machine *sm.StateMachine, sim *simulator.Simulator) *gatt.Service {
	controlService := gatt.NewService(attrControlServiceUUID)

	dec := csafe.Decoder{}
	enc := csafe.Encoder{}
	responses := make(chan []byte, 8)
	var frameToggle uint32 // low bit alternates per response, per FRAMECNT_FLG

	/*
		C2 PM receive characteristic (write-only per spec)
	*/
	receiveChar := controlService.AddCharacteristic(attrReceiveCharacteristicsUUID)
	receiveChar.HandleWriteFunc(func(r gatt.Request, data []byte) (status byte) {
		frame, ok := csafe.ExtractFrame(data)
		if !ok {
			logrus.Warnf("[[Control]] no CSAFE frame found in write %v", data)
			return gatt.StatusUnexpectedError
		}

		packets, err := dec.DecodeAll(frame)
		if err != nil {
			logrus.Warnf("[[Control]] failed to decode CSAFE frame %v: %v", frame, err)
			return gatt.StatusUnexpectedError
		}

		for _, pkt := range packets {
			cmd := pkt.Cmds[0]
			logrus.Infof("[[Control]] decoded command 0x%x data=%v", cmd, pkt.Data)
			handleCommand(machine, sim, cmd, pkt.Data)
		}

		// A response frame carries one status byte for the whole incoming
		// write (which may itself have chained several commands), not one
		// per command -- per the spec's own worked examples (Figure 8/9),
		// a response to e.g. GOINUSE (`F1 85 85 F2`) is just that single
		// status byte reframed (`F1 <status> <status> F2`), not a longer
		// frame echoing the command back.
		var toggle byte
		if atomic.AddUint32(&frameToggle, 1)&1 == 1 {
			toggle = csafe.FRAMECNT_FLG
		}
		resp := enc.Encode(csafe.Packet{
			Cmds:    []byte{toggle | statusByte(machine)},
			JustCmd: true,
		})
		select {
		case responses <- resp:
		default:
			logrus.Warn("[[Control]] response queue full, dropping a CSAFE response")
		}

		return gatt.StatusSuccess
	})

	/*
		C2 PM transmit characteristic: pushes one CSAFE response per
		processed command, produced by the receive handler above.
	*/
	transmitChar := controlService.AddCharacteristic(attrTransmitCharacteristicsUUID)
	transmitChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("[[Transmit]] client subscribed")
		for {
			if n.Done() {
				return
			}
			select {
			case resp := <-responses:
				if _, err := writeNotification(n, resp); err != nil {
					logrus.Warnf("[[Transmit]] write failed, stopping notify loop: %v", err)
					return
				}
			case <-time.After(responsePollInterval):
			}
		}
	})

	return controlService
}

// handleCommand applies the effect of a single decoded CSAFE command:
// control commands (GOIDLE/GOHAVEID/GOINUSE/...) drive the shared state
// machine, workout-configuration commands (SETTWORK/SETHORIZONTAL/...)
// configure the simulator, and everything else is accepted as a no-op,
// matching how a real PM5 tolerates commands it doesn't specifically act on.
func handleCommand(machine *sm.StateMachine, sim *simulator.Simulator, cmd byte, data []byte) {
	switch cmd {
	case byte(csafe.RESET_CMD):
		machine.Reset()
		sim.Reset()
		sim.RequestRestart()
	case byte(csafe.GOIDLE_CMD), byte(csafe.GOHAVEID_CMD), byte(csafe.GOINUSE_CMD),
		byte(csafe.GOFINISHED_CMD), byte(csafe.GOREADY_CMD):
		if err := machine.Update(cmd); err != nil {
			logrus.Debugf("[[Control]] cmd 0x%x rejected in state %s: %v", cmd, machine.GetStateName(), err)
		} else if cmd == byte(csafe.GOINUSE_CMD) {
			// Always (re)start CSV playback from the first stroke on a
			// fresh transition into INUSE, regardless of wherever a prior
			// pass happened to be.
			sim.RequestRestart()
		}
	case byte(csafe.SETPMCFG_CMD):
		applyPMConfig(machine, sim, data)
	case byte(csafe.SETTWORK_CMD):
		applySetTwork(sim, data)
	case byte(csafe.SETHORIZONTAL_CMD):
		applySetHorizontal(sim, data)
	case byte(csafe.SETPROGRAM_CMD):
		// Program-based workouts aren't modeled in detail; fall back to an
		// unlimited "Just Row" so the simulator still produces sane data.
		sim.ConfigureWorkout(simulator.WorkoutConfig{WorkoutType: config.WORKOUTTYPE_JUSTROW_NOSPLITS})
	default:
		// GETSTATUS/GETVERSION/audio/text/... commands: acknowledged (a
		// response is still sent by the caller) but otherwise a no-op.
	}
}

// applySetTwork configures a fixed-time workout from a SETTWORK_CMD payload.
// Per the Hours/Minutes/Seconds triplet convention used elsewhere in this
// CSAFE command set (see HMS_FORMAT_CNT in csafe-defs.go), the payload is
// [hours, minutes, seconds].
func applySetTwork(sim *simulator.Simulator, data []byte) {
	if len(data) < 3 {
		return
	}
	duration := time.Duration(data[0])*time.Hour +
		time.Duration(data[1])*time.Minute +
		time.Duration(data[2])*time.Second

	sim.ConfigureWorkout(simulator.WorkoutConfig{
		WorkoutType:    config.WORKOUTTYPE_FIXEDTIME_NOSPLITS,
		DurationType:   config.CSAFE_TIME_DURATION,
		TargetDuration: duration,
	})
}

// applySetHorizontal configures a fixed-distance workout from a
// SETHORIZONTAL_CMD payload. This emulator's bundled spec docs only cover
// the PM5-specific BLE characteristics, not the generic CSAFE Protocol
// Technical Spec that defines this command's wire format, so this is a
// reasonable best-effort decode (big-endian meters, matching this codebase's
// existing csafe.Encoder.getBytesArray convention) rather than a verified
// one.
func applySetHorizontal(sim *simulator.Simulator, data []byte) {
	if len(data) < 2 {
		return
	}
	distance := float64(uint16(data[0])<<8 | uint16(data[1]))

	sim.ConfigureWorkout(simulator.WorkoutConfig{
		WorkoutType:    config.WORKOUTTYPE_FIXEDDIST_NOSPLITS,
		DurationType:   config.CSAFE_DISTANCE_DURATION,
		TargetDistance: distance,
	})
}

// statusByte builds a CSAFE response status byte: frame-status nibble
// (PREVOK_FLG, since a decoded-but-unsupported command is still accepted)
// combined with the current PM5 slave state nibble.
func statusByte(machine *sm.StateMachine) byte {
	return csafe.PREVOK_FLG | slaveStateFlag(machine)
}

func slaveStateFlag(machine *sm.StateMachine) byte {
	switch machine.GetStateName() {
	case config.PM5_STATE_READY:
		return csafe.SLAVESTATE_RDY_FLG
	case config.PM5_STATE_IDLE:
		return csafe.SLAVESTATE_IDLE_FLG
	case config.PM5_STATE_HAVEID:
		return csafe.SLAVESTATE_HAVEID_FLG
	case config.PM5_STATE_INUSE:
		return csafe.SLAVESTATE_INUSE_FLG
	case config.PM5_STATE_PAUSED:
		return csafe.SLAVESTATE_PAUSE_FLG
	case config.PM5_STATE_FINISHED:
		return csafe.SLAVESTATE_FINISH_FLG
	case config.PM5_STATE_MANUAL:
		return csafe.SLAVESTATE_MANUAL_FLG
	default:
		return csafe.SLAVESTATE_ERR_FLG
	}
}

// Screen type/value codes for the CSAFE_PM_SET_SCREENSTATE command (see the Concept2 PM CSAFE
// Communication Definition, "Screen Type" and "Screen Value (Workout Type)").
const (
	screenTypeWorkout           = 0x01
	screenValueTerminateWorkout = 0x02
	screenValuePrepareToRow     = 0x01
)

// Commands inside the C2 proprietary SETPMCFG wrapper that configure a workout, and the unit
// identifiers of a workout duration.
const (
	pmSetWorkoutType          = 0x01
	pmSetWorkoutDuration      = 0x03
	pmSetRestDuration         = 0x04
	pmSetIntervalType         = 0x17 // CSAFE_PM_SET_INTERVALTYPE
	pmSetWorkoutIntervalCount = 0x18 // CSAFE_PM_SET_WORKOUTINTERVALCOUNT

	durationIdentifierTime     = 0x00 // hundredths of a second
	durationIdentifierCalories = 0x40
	durationIdentifierDistance = 0x80 // meters
)

// pmWorkoutBuilder accumulates a workout configuration across the sequence of SETPMCFG
// sub-commands a client sends -- each in its own CSAFE frame -- until SET_SCREENSTATE /
// PREPARETOROWWORKOUT starts it (see applyPMConfig/startWorkout). A fresh one begins each time
// SET_WORKOUTTYPE arrives, since that's always the first command of a new configuration sequence.
//
// CSAFE_PM_SET_WORKOUTINTERVALCOUNT is reused by the wire protocol for two different purposes,
// disambiguated here by workout type, per the two documented examples in the bundled CSAFE spec
// ("Fixed Distance Interval" and "Variable Interval" in
// docs/resources/Concept2 PM CSAFE Communication Definition.pdf): for a variable interval workout
// it's the 0-based index of the interval group currently being configured, sent once per
// interval, each followed by its own type/duration/rest and committed by its own
// CONFIGURE_WORKOUT. There's no documented example of an explicit finite count for a
// constant/single interval workout (the simplest example never sends this command at all, and
// repeats forever until Terminate); by analogy with the variable case, this emulator treats a
// value sent there as "repeat this one interval this many times" instead -- the one interpretive
// call in this implementation where the bundled spec doesn't fully pin down the wire format.
type pmWorkoutBuilder struct {
	workoutType byte

	current      simulator.IntervalSpec // the interval (or flat goal) currently being configured
	pendingCount int                    // last SET_WORKOUTINTERVALCOUNT seen since current started; -1 = none seen

	// Committed intervals: for a variable workout, one entry per interval, indexed by
	// pendingCount at commit time; for a constant/single workout, just the one entry.
	intervals   []simulator.IntervalSpec
	repeatCount int // constant/single workouts only; 0 = no explicit count (repeat until Terminate)
}

func newPMWorkoutBuilder() *pmWorkoutBuilder {
	return &pmWorkoutBuilder{pendingCount: -1}
}

func isIntervalWorkoutType(t byte) bool {
	switch t {
	case config.WORKOUTTYPE_FIXEDTIME_INTERVAL, config.WORKOUTTYPE_FIXEDDIST_INTERVAL, config.WORKOUTTYPE_FIXEDCALS_INTERVAL,
		config.WORKOUTTYPE_VARIABLE_INTERVAL, config.WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL:
		return true
	}
	return false
}

// isRestUndefinedIntervalType reports whether an interval type (OBJ_INTERVALTYPE_T) is one of the
// "rest undefined" variants.
func isRestUndefinedIntervalType(t byte) bool {
	switch t {
	case config.INTERVALTYPE_TIMERESTUNDEFINED, config.INTERVALTYPE_DISTANCERESTUNDEFINED,
		config.INTERVALTYPE_RESTUNDEFINED, config.INTERVALTYPE_CALRESTUNDEFINED,
		config.INTERVALTYPE_WATTMINUTERESTUNDEFINED:
		return true
	}
	return false
}

func isVariableWorkoutType(t byte) bool {
	return t == config.WORKOUTTYPE_VARIABLE_INTERVAL || t == config.WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL
}

// commitInterval finalizes the interval currently being configured, in response to
// CSAFE_PM_CONFIGURE_WORKOUT/Programming mode enable (sent once per interval for a variable
// workout, or once overall for a single/constant one -- see the type doc comment above). Must be
// called with pmWorkoutMu held.
func (w *pmWorkoutBuilder) commitInterval() {
	// A client that sets the undefined-rest type first makes every interval undefined-rest; the
	// spec's own sequence instead marks each interval by its interval type (see SET_INTERVALTYPE)
	// and only switches the workout type once all intervals are configured.
	if w.workoutType == config.WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL {
		w.current.RestUndefined = true
	}

	if isVariableWorkoutType(w.workoutType) {
		idx := 0
		if w.pendingCount >= 0 {
			idx = w.pendingCount
		}
		for len(w.intervals) <= idx {
			w.intervals = append(w.intervals, simulator.IntervalSpec{})
		}
		w.intervals[idx] = w.current
		w.current = simulator.IntervalSpec{}
		w.pendingCount = -1
		return
	}
	if isIntervalWorkoutType(w.workoutType) && w.pendingCount >= 0 {
		w.repeatCount = w.pendingCount
	}
	// Flat (non-interval) and constant-interval workouts: startWorkout reads w.current directly,
	// nothing more to commit here.
}

// The workout a client is currently building; kept between frames until the workout is started
// (SCREENVALUEWORKOUT_PREPARETOROWWORKOUT), at which point startWorkout consumes and resets it.
var (
	pmWorkoutMu sync.Mutex
	pmWorkout   = newPMWorkoutBuilder()
)

// applyPMConfig handles the C2 proprietary SETPMCFG wrapper, whose payload is a sequence of
// [command, byte count, data...] entries:
//   - SET_WORKOUTTYPE, SET_WORKOUTDURATION, SET_RESTDURATION and SET_WORKOUTINTERVALCOUNT
//     configure the workout (or, for a variable workout, one interval of it at a time -- see
//     pmWorkoutBuilder) to be started.
//   - SET_SCREENSTATE with SCREENTYPE_WORKOUT/PREPARETOROWWORKOUT starts that workout, as the
//     PM does once the rower begins pulling.
//   - SET_SCREENSTATE with SCREENTYPE_WORKOUT/TERMINATEWORKOUT is the "Terminate Workout"
//     command, which per the spec's PM state transitions takes a running workout through
//     Terminate -> Rearm -> WaitToBegin, i.e. back to the idle state, which also stops the
//     simulator.
func applyPMConfig(machine *sm.StateMachine, sim *simulator.Simulator, data []byte) {
	for i := 0; i+1 < len(data); {
		cmd, n := data[i], int(data[i+1])
		if i+2+n > len(data) {
			return
		}
		payload := data[i+2 : i+2+n]
		i += 2 + n

		switch {
		case cmd == pmSetWorkoutType && len(payload) >= 1:
			pmWorkoutMu.Lock()
			if payload[0] == config.WORKOUTTYPE_VARIABLE_UNDEFINEDREST_INTERVAL &&
				isVariableWorkoutType(pmWorkout.workoutType) && len(pmWorkout.intervals) > 0 {
				// The spec programs a variable workout that has undefined rests as variable
				// intervals, then changes the type to undefined-rest once they are all configured
				// (see "Variable Interval Undefined Rest"); that must not discard them.
				pmWorkout.workoutType = payload[0]
			} else {
				pmWorkout = newPMWorkoutBuilder()
				pmWorkout.workoutType = payload[0]
			}
			pmWorkoutMu.Unlock()
		case cmd == pmSetIntervalType && len(payload) >= 1:
			pmWorkoutMu.Lock()
			pmWorkout.current.RestUndefined = isRestUndefinedIntervalType(payload[0])
			pmWorkoutMu.Unlock()
		case cmd == pmSetWorkoutDuration && len(payload) >= 5:
			applyPMWorkoutDuration(payload)
		case cmd == pmSetRestDuration && len(payload) >= 2:
			pmWorkoutMu.Lock()
			pmWorkout.current.RestDuration = time.Duration(uint16(payload[0])<<8|uint16(payload[1])) * time.Second
			pmWorkoutMu.Unlock()
		case cmd == pmSetWorkoutIntervalCount && len(payload) >= 1:
			pmWorkoutMu.Lock()
			pmWorkout.pendingCount = int(payload[0])
			pmWorkoutMu.Unlock()
		case cmd == byte(csafe.PM_CONFIGURE_WORKOUT) && len(payload) >= 1 && payload[0] == 1:
			pmWorkoutMu.Lock()
			pmWorkout.commitInterval()
			pmWorkoutMu.Unlock()
		case cmd == byte(csafe.PM_SET_SCREENSTATE) && len(payload) >= 2 && payload[0] == screenTypeWorkout:
			switch payload[1] {
			case screenValueTerminateWorkout:
				terminateWorkout(machine, sim)
			case screenValuePrepareToRow:
				startWorkout(machine, sim)
			}
		}
	}
}

// applyPMWorkoutDuration records the goal of a SET_WORKOUTDURATION payload: a unit identifier
// followed by a big-endian 32-bit value.
func applyPMWorkoutDuration(payload []byte) {
	value := uint32(payload[1])<<24 | uint32(payload[2])<<16 | uint32(payload[3])<<8 | uint32(payload[4])

	pmWorkoutMu.Lock()
	defer pmWorkoutMu.Unlock()
	switch payload[0] {
	case durationIdentifierTime:
		pmWorkout.current.DurationType = config.CSAFE_TIME_DURATION
		pmWorkout.current.TargetDuration = time.Duration(value) * 10 * time.Millisecond
	case durationIdentifierDistance:
		pmWorkout.current.DurationType = config.CSAFE_DISTANCE_DURATION
		pmWorkout.current.TargetDistance = float64(value)
	case durationIdentifierCalories:
		pmWorkout.current.DurationType = config.CSAFE_CALORIES_DURATION
		pmWorkout.current.TargetCalories = float64(value)
	}
}

// startWorkout applies the configured workout and starts it from the beginning, whatever state
// the PM was in.
func startWorkout(machine *sm.StateMachine, sim *simulator.Simulator) {
	pmWorkoutMu.Lock()
	w := pmWorkout
	pmWorkout = newPMWorkoutBuilder()
	pmWorkoutMu.Unlock()

	cfg := simulator.WorkoutConfig{WorkoutType: w.workoutType}
	switch {
	case isVariableWorkoutType(w.workoutType):
		cfg.Intervals = append([]simulator.IntervalSpec(nil), w.intervals...)
	case isIntervalWorkoutType(w.workoutType):
		if w.repeatCount > 1 {
			cfg.Intervals = make([]simulator.IntervalSpec, w.repeatCount)
			for i := range cfg.Intervals {
				cfg.Intervals[i] = w.current
			}
		} else {
			cfg.Intervals = []simulator.IntervalSpec{w.current}
			cfg.Repeat = w.repeatCount == 0 // no explicit count: repeat until Terminate, matching a real PM5
		}
	default:
		cfg.DurationType = w.current.DurationType
		cfg.TargetDistance = w.current.TargetDistance
		cfg.TargetDuration = w.current.TargetDuration
	}
	sim.ConfigureWorkout(cfg)

	if machine.GetStateName() == config.PM5_STATE_FINISHED {
		// The finished state does move to idle, but reports an error while doing so.
		_ = machine.Update(config.CSAFE_GOIDLE_CMD)
	}
	if err := machine.Update(config.CSAFE_GOINUSE_CMD); err != nil {
		logrus.Debugf("[[Control]] start: GOINUSE rejected in state %s: %v", machine.GetStateName(), err)
	}
	sim.RequestRestart()
}

// terminateWorkout ends the current workout and rearms the PM to the idle ("wait to begin")
// state, whatever state it was in.
func terminateWorkout(machine *sm.StateMachine, sim *simulator.Simulator) {
	if machine.GetStateName() == config.PM5_STATE_INUSE || machine.GetStateName() == config.PM5_STATE_PAUSED {
		if err := machine.Update(config.CSAFE_GOFINISHED_CMD); err != nil {
			logrus.Debugf("[[Control]] terminate: GOFINISHED rejected in state %s: %v", machine.GetStateName(), err)
		}
	}
	if err := machine.Update(config.CSAFE_GOIDLE_CMD); err != nil {
		logrus.Debugf("[[Control]] terminate: GOIDLE rejected in state %s: %v", machine.GetStateName(), err)
	}
	sim.Reset()
}
