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
		{"AdditionalStatus3", EncodeAdditionalStatus3(s), 13},
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

func TestEncodeAdditionalStatus3_FieldOrder(t *testing.T) {
	s := simulator.LiveState{
		OperationalState:  2,
		VerificationState: 1,
		ScreenNumber:      1011,
		LastError:         1840,
		GameID:            5,
		GameScore:         300,
		BatteryLevel:      90,
	}

	got := EncodeAdditionalStatus3(s)
	want := []byte{
		2, 1, // operational state, verification state
		0xF3, 0x03, // screen number = 1011 (0x03F3)
		0x30, 0x07, // last error = 1840 (0x0730)
		0, 0, 0, // reserved
		5,          // game id
		0x2C, 0x01, // game score = 300 (0x012C)
		90, // battery level
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

// reassembleForceCurve is a Go port of the reference Dart ForceCurveAssembler
// used to verify EncodeForceCurvePackets' output is actually consumable by a
// real client: it reassembles a curve from its packets the same way.
func reassembleForceCurve(t *testing.T, packets [][]byte) []uint16 {
	t.Helper()
	var points []uint16
	for _, pkt := range packets {
		if len(pkt) < 2 {
			t.Fatalf("packet too short: %v", pkt)
		}
		pointCount := int(pkt[0] & 0x0F)
		for i := 0; i < pointCount; i++ {
			off := 2 + 2*i
			if off+1 >= len(pkt) {
				t.Fatalf("packet truncated: %v", pkt)
			}
			points = append(points, uint16(pkt[off])|uint16(pkt[off+1])<<8)
		}
	}
	return points
}

func TestEncodeForceCurvePackets_RoundTrip(t *testing.T) {
	points := make([]uint16, 0, 57)
	for i := 0; i < 57; i++ {
		points = append(points, uint16(i*3))
	}

	packets := EncodeForceCurvePackets(points, 9)

	wantTotal := (len(points) + 8) / 9
	if len(packets) != wantTotal {
		t.Fatalf("packet count = %d, want %d", len(packets), wantTotal)
	}
	for seq, pkt := range packets {
		totalPackets := int(pkt[0] >> 4)
		if totalPackets != wantTotal {
			t.Errorf("packet %d: header total = %d, want %d", seq, totalPackets, wantTotal)
		}
		if int(pkt[1]) != seq {
			t.Errorf("packet %d: seq byte = %d, want %d", seq, pkt[1], seq)
		}
	}

	got := reassembleForceCurve(t, packets)
	if len(got) != len(points) {
		t.Fatalf("reassembled %d points, want %d", len(got), len(points))
	}
	for i := range points {
		if got[i] != points[i] {
			t.Errorf("point[%d] = %d, want %d", i, got[i], points[i])
		}
	}
}

func TestEncodeForceCurvePackets_ClampsPacketSize(t *testing.T) {
	packets := EncodeForceCurvePackets([]uint16{1, 2, 3}, 0)
	if len(packets) != 3 {
		t.Fatalf("expected maxPointsPerPacket to clamp to 1, got %d packets", len(packets))
	}
	packets = EncodeForceCurvePackets([]uint16{1, 2, 3}, 99)
	if len(packets) != 1 || packets[0][0]&0x0F != 3 {
		t.Fatalf("expected maxPointsPerPacket to clamp to 15, got %v", packets)
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
