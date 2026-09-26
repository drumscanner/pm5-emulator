package service

import (
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
