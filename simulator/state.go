package simulator

import "time"

// LiveState is a snapshot of everything the rowing service characteristics
// need to encode. All physical quantities are kept in natural SI-ish units
// (meters, seconds, watts, ...); protocol/rowing is responsible for scaling
// them into the fixed-point byte layouts the PM5 BLE spec defines.
type LiveState struct {
	// Workout / general status
	ElapsedTime         time.Duration
	Distance            float64 // meters
	WorkoutType         byte
	IntervalType        byte
	WorkoutState        byte
	RowingState         byte
	StrokeState         byte
	TotalWorkDistance   float64 // meters, target distance (0 if unset)
	WorkoutDuration     time.Duration
	WorkoutDurationType byte
	DragFactor          byte

	// Additional status 1
	Speed        float64       // m/s
	StrokeRate   byte          // strokes/minute
	HeartRate    byte          // bpm, 255 = invalid/no belt
	CurrentPace  time.Duration // per 500m
	AvgPace      time.Duration // per 500m
	RestDistance float64
	RestTime     time.Duration

	// Additional status 2
	IntervalCount     byte
	AveragePower      float64 // watts
	TotalCalories     float64
	SplitAvgPace      time.Duration
	SplitAvgPower     float64
	SplitAvgCalories  float64 // cal/hr
	LastSplitTime     time.Duration
	LastSplitDistance float64

	// Stroke data
	DriveLength        float64 // meters
	DriveTime          time.Duration
	StrokeRecoveryTime time.Duration
	StrokeDistance     float64 // meters covered during the last stroke
	PeakDriveForce     float64 // lbf
	AvgDriveForce      float64 // lbf
	WorkPerStroke      float64 // joules
	StrokeCount        uint16

	// Additional stroke data
	StrokePower           float64 // watts
	StrokeCalories        float64 // cal/hr
	ProjectedWorkTime     time.Duration
	ProjectedWorkDistance float64

	// Split/interval data
	SplitIntervalTime     time.Duration
	SplitIntervalDistance float64
	IntervalRestTime      time.Duration
	IntervalRestDistance  float64
	SplitIntervalType     byte
	SplitIntervalNumber   byte

	// Additional split/interval data
	SplitAvgStrokeRate byte
	SplitWorkHeartrate byte
	SplitRestHeartrate byte
	SplitSpeed         float64
	SplitPower         float64
	SplitAvgDragFactor byte

	// End of workout summary
	AvgStrokeRate     byte
	EndingHeartrate   byte
	AvgHeartrate      byte
	MinHeartrate      byte
	MaxHeartrate      byte
	DragFactorAvg     byte
	RecoveryHeartRate byte

	// Additional end of workout summary
	SplitIntervalSize  uint16
	SplitIntervalCount byte
	TotalRestDistance  float64
	AvgCalories        float64 // cal/hr

	// Additional status 3 (0x003E, newer PM5 firmware only)
	OperationalState  byte
	VerificationState byte
	ScreenNumber      uint16
	LastError         uint16
	GameID            byte
	GameScore         uint16
	BatteryLevel      byte
}
