// Package rowing encodes simulator.LiveState into the exact byte layouts the
// Concept2 PM5 Bluetooth Smart Communication Interface Definition (rev 1.25)
// specifies for the C2 Rowing service (0x0030) characteristics.
package rowing

import (
	"time"

	"pm5-emulator/simulator"
)

// Characteristic identifier bytes, used both as the notify-once byte for
// direct characteristic reads/notifies and as the ID prefix byte on the
// multiplexed information characteristic (0x0080).
const (
	IDGeneralStatus                 = 0x31
	IDAdditionalStatus1             = 0x32
	IDAdditionalStatus2             = 0x33
	IDStrokeData                    = 0x35
	IDAdditionalStrokeData          = 0x36
	IDSplitIntervalData             = 0x37
	IDAdditionalSplitIntervalData   = 0x38
	IDEndOfWorkoutSummary           = 0x39
	IDAdditionalEndOfWorkoutSummary = 0x3A
)

// --- small helpers -----------------------------------------------------

// u16le/u24le/u32le append little-endian, spec ("Lo"/"Mid"/"High") ordered
// fixed-width integers.
func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func appendU24(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16))
}

func clampU8(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func clampU16(v float64) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

func clampU24(v float64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 16777215 {
		return 16777215
	}
	return uint32(v)
}

func centiseconds(d time.Duration) float64 { return d.Seconds() * 100 }
func deciseconds(d time.Duration) float64  { return d.Seconds() * 10 }
func seconds(d time.Duration) float64      { return d.Seconds() }

// --- 0x0031 C2 rowing general status (19 bytes) -------------------------

func EncodeGeneralStatus(s simulator.LiveState) []byte {
	b := make([]byte, 0, 19)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU24(b, clampU24(s.Distance*10))
	b = append(b, s.WorkoutType, s.IntervalType, s.WorkoutState, s.RowingState, s.StrokeState)
	b = appendU24(b, clampU24(s.TotalWorkDistance))
	b = appendU24(b, clampU24(centiseconds(s.WorkoutDuration)))
	b = append(b, s.WorkoutDurationType, s.DragFactor)
	return b
}

// --- 0x0032 C2 rowing additional status 1 (16 bytes) --------------------

func EncodeAdditionalStatus1(s simulator.LiveState) []byte {
	b := make([]byte, 0, 16)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU16(b, clampU16(s.Speed*1000))
	b = append(b, s.StrokeRate, s.HeartRate)
	b = appendU16(b, clampU16(centiseconds(s.CurrentPace)))
	b = appendU16(b, clampU16(centiseconds(s.AvgPace)))
	b = appendU16(b, clampU16(s.RestDistance))
	b = appendU24(b, clampU24(centiseconds(s.RestTime)))
	return b
}

// --- 0x0033 C2 rowing additional status 2 (20 bytes) ---------------------
// Note: the spec's own multiplexed-information table (Table 4) lists this
// characteristic's byte length as 18, which disagrees with its own field
// list (as does the standalone characteristic table, which says 20 bytes)
// -- 20 is what the field-by-field listing sums to, so that's what we use.

func EncodeAdditionalStatus2(s simulator.LiveState) []byte {
	b := make([]byte, 0, 20)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = append(b, s.IntervalCount)
	b = appendU16(b, clampU16(s.AveragePower))
	b = appendU16(b, clampU16(s.TotalCalories))
	b = appendU16(b, clampU16(centiseconds(s.SplitAvgPace)))
	b = appendU16(b, clampU16(s.SplitAvgPower))
	b = appendU16(b, clampU16(s.SplitAvgCalories))
	b = appendU24(b, clampU24(deciseconds(s.LastSplitTime)))
	b = appendU24(b, clampU24(s.LastSplitDistance))
	return b
}

// --- 0x0035 C2 rowing stroke data (20 bytes) -----------------------------

func EncodeStrokeData(s simulator.LiveState) []byte {
	b := make([]byte, 0, 20)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU24(b, clampU24(s.Distance*10))
	b = append(b, clampU8(s.DriveLength*100), clampU8(centiseconds(s.DriveTime)))
	b = appendU16(b, clampU16(centiseconds(s.StrokeRecoveryTime)))
	b = appendU16(b, clampU16(s.StrokeDistance*100))
	b = appendU16(b, clampU16(s.PeakDriveForce*10))
	b = appendU16(b, clampU16(s.AvgDriveForce*10))
	b = appendU16(b, clampU16(s.WorkPerStroke*10))
	b = appendU16(b, s.StrokeCount)
	return b
}

// --- 0x0036 C2 rowing additional stroke data (15 bytes) -------------------

func EncodeAdditionalStrokeData(s simulator.LiveState) []byte {
	b := make([]byte, 0, 15)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU16(b, clampU16(s.StrokePower))
	b = appendU16(b, clampU16(s.StrokeCalories))
	b = appendU16(b, s.StrokeCount)
	b = appendU24(b, clampU24(seconds(s.ProjectedWorkTime)))
	b = appendU24(b, clampU24(s.ProjectedWorkDistance))
	return b
}

// --- 0x0037 C2 rowing split/interval data (18 bytes) -----------------------

func EncodeSplitIntervalData(s simulator.LiveState) []byte {
	b := make([]byte, 0, 18)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU24(b, clampU24(s.Distance*10))
	b = appendU24(b, clampU24(deciseconds(s.SplitIntervalTime)))
	b = appendU24(b, clampU24(s.SplitIntervalDistance))
	b = appendU16(b, clampU16(seconds(s.IntervalRestTime)))
	b = appendU16(b, clampU16(s.IntervalRestDistance))
	b = append(b, s.SplitIntervalType, s.SplitIntervalNumber)
	return b
}

// --- 0x0038 C2 rowing additional split/interval data (18 bytes) ------------

func EncodeAdditionalSplitIntervalData(s simulator.LiveState) []byte {
	b := make([]byte, 0, 18)
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = append(b, s.SplitAvgStrokeRate, s.SplitWorkHeartrate, s.SplitRestHeartrate)
	b = appendU16(b, clampU16(deciseconds(s.SplitAvgPace)))
	b = appendU16(b, clampU16(s.TotalCalories))
	b = appendU16(b, clampU16(s.SplitAvgCalories))
	b = appendU16(b, clampU16(s.SplitSpeed*1000))
	b = appendU16(b, clampU16(s.SplitPower))
	b = append(b, s.SplitAvgDragFactor, s.SplitIntervalNumber)
	return b
}

// --- 0x0039 C2 rowing end of workout summary data (20 bytes) ---------------

func EncodeEndOfWorkoutSummary(s simulator.LiveState) []byte {
	b := make([]byte, 0, 20)
	b = appendU16(b, 0) // Log Entry Date: no RTC/log storage in this emulator
	b = appendU16(b, 0) // Log Entry Time
	b = appendU24(b, clampU24(centiseconds(s.ElapsedTime)))
	b = appendU24(b, clampU24(s.Distance*10))
	b = append(b, s.AvgStrokeRate, s.EndingHeartrate, s.AvgHeartrate, s.MinHeartrate, s.MaxHeartrate)
	b = append(b, s.DragFactorAvg, s.RecoveryHeartRate, s.WorkoutType)
	b = appendU16(b, clampU16(deciseconds(s.AvgPace)))
	return b
}

// --- 0x003A C2 rowing end of workout additional summary data (19 bytes) ----

func EncodeAdditionalEndOfWorkoutSummary(s simulator.LiveState) []byte {
	b := make([]byte, 0, 19)
	b = appendU16(b, 0) // Log Entry Date
	b = appendU16(b, 0) // Log Entry Time
	b = append(b, s.SplitIntervalType)
	b = appendU16(b, s.SplitIntervalSize)
	b = append(b, s.SplitIntervalCount)
	b = appendU16(b, clampU16(s.TotalCalories))
	b = appendU16(b, clampU16(s.AveragePower))
	b = appendU24(b, clampU24(s.TotalRestDistance))
	b = appendU16(b, clampU16(seconds(s.RestTime)))
	b = appendU16(b, clampU16(s.AvgCalories))
	return b
}

// encoders maps a multiplexed-information ID byte to its encode function.
var encoders = map[byte]func(simulator.LiveState) []byte{
	IDGeneralStatus:                 EncodeGeneralStatus,
	IDAdditionalStatus1:             EncodeAdditionalStatus1,
	IDAdditionalStatus2:             EncodeAdditionalStatus2,
	IDStrokeData:                    EncodeStrokeData,
	IDAdditionalStrokeData:          EncodeAdditionalStrokeData,
	IDSplitIntervalData:             EncodeSplitIntervalData,
	IDAdditionalSplitIntervalData:   EncodeAdditionalSplitIntervalData,
	IDEndOfWorkoutSummary:           EncodeEndOfWorkoutSummary,
	IDAdditionalEndOfWorkoutSummary: EncodeAdditionalEndOfWorkoutSummary,
}

// EncodeMultiplexed returns the ID-prefixed payload for the 0x0080
// multiplexed information characteristic, given one of the ID* constants.
func EncodeMultiplexed(id byte, s simulator.LiveState) []byte {
	enc, ok := encoders[id]
	if !ok {
		return nil
	}
	return append([]byte{id}, enc(s)...)
}
