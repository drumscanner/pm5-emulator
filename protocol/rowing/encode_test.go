package rowing

import (
	"testing"
	"time"

	"pm5-emulator/simulator"
)

// lengths pins the exact byte lengths documented in the PM5 Bluetooth Smart
// Communication Interface Definition (rev 1.25) for each characteristic.
func TestEncode_Lengths(t *testing.T) {
	s := simulator.LiveState{
		ElapsedTime: 12345 * time.Millisecond,
		Distance:    1234.5,
		CurrentPace: 100 * time.Second,
		AvgPace:     100 * time.Second,
	}

	tests := []struct {
		name string
		got  []byte
		want int
	}{
		{"GeneralStatus", EncodeGeneralStatus(s), 19},
		{"AdditionalStatus1", EncodeAdditionalStatus1(s), 16},
		{"AdditionalStatus2", EncodeAdditionalStatus2(s), 20},
		{"StrokeData", EncodeStrokeData(s), 20},
		{"AdditionalStrokeData", EncodeAdditionalStrokeData(s), 15},
		{"SplitIntervalData", EncodeSplitIntervalData(s), 18},
		{"AdditionalSplitIntervalData", EncodeAdditionalSplitIntervalData(s), 18},
		{"EndOfWorkoutSummary", EncodeEndOfWorkoutSummary(s), 20},
		{"AdditionalEndOfWorkoutSummary", EncodeAdditionalEndOfWorkoutSummary(s), 19},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.got) != tt.want {
				t.Errorf("%s length = %d, want %d", tt.name, len(tt.got), tt.want)
			}
		})
	}
}

func TestEncodeGeneralStatus_FieldOrder(t *testing.T) {
	s := simulator.LiveState{
		ElapsedTime:         10 * time.Second, // 1000 centiseconds -> 0x03E8
		Distance:            50,               // 500 (0.1m units) -> 0x01F4
		WorkoutType:         1,
		IntervalType:        2,
		WorkoutState:        3,
		RowingState:         1,
		StrokeState:         2,
		TotalWorkDistance:   2000,
		WorkoutDuration:     60 * time.Second, // 6000 centiseconds
		WorkoutDurationType: 0,
		DragFactor:          130,
	}

	got := EncodeGeneralStatus(s)
	want := []byte{
		0xE8, 0x03, 0x00, // elapsed time = 1000 (0x03E8)
		0xF4, 0x01, 0x00, // distance = 500 (0x01F4)
		1, 2, 3, 1, 2, // workout type, interval type, workout state, rowing state, stroke state
		0xD0, 0x07, 0x00, // total work distance = 2000 (0x07D0)
		0x70, 0x17, 0x00, // workout duration = 6000 (0x1770)
		0, 130, // duration type, drag factor
	}

	if len(got) != len(want) {
		t.Fatalf("length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("byte[%d] = 0x%02X, want 0x%02X (got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

func TestEncodeMultiplexed_PrefixesID(t *testing.T) {
	s := simulator.LiveState{}
	got := EncodeMultiplexed(IDGeneralStatus, s)
	if len(got) != 1+19 {
		t.Fatalf("length = %d, want %d", len(got), 1+19)
	}
	if got[0] != IDGeneralStatus {
		t.Errorf("id byte = 0x%02X, want 0x%02X", got[0], IDGeneralStatus)
	}

	if EncodeMultiplexed(0xFF, s) != nil {
		t.Errorf("expected nil for unknown id")
	}
}
