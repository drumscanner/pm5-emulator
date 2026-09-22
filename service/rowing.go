package service

import (
	"sync/atomic"
	"time"

	"pm5-emulator/protocol/rowing"
	"pm5-emulator/simulator"

	"github.com/bettercap/gatt"
	"github.com/sirupsen/logrus"
)

/*
	C2 rowing primary service
*/

// C2 rowing primary service and characteristics UUIDs
var (
	attrRowingServiceUUID, _                                    = gatt.ParseUUID(getFullUUID("0030"))
	attrGeneralStatusCharacteristicsUUID, _                     = gatt.ParseUUID(getFullUUID("0031"))
	attrAdditionalStatus1CharacteristicsUUID, _                 = gatt.ParseUUID(getFullUUID("0032"))
	attrAdditionalStatus2CharacteristicsUUID, _                 = gatt.ParseUUID(getFullUUID("0033"))
	attrSampleRateCharacteristicsUUID, _                        = gatt.ParseUUID(getFullUUID("0034"))
	attrStrokeDataCharacteristicsUUID, _                        = gatt.ParseUUID(getFullUUID("0035"))
	attrAdditionalStrokeDataCharacteristicsUUID, _              = gatt.ParseUUID(getFullUUID("0036"))
	attrSplitIntervalDataCharacteristicsUUID, _                 = gatt.ParseUUID(getFullUUID("0037"))
	attrAdditionalSplitIntervalDataCharacteristicsUUID, _       = gatt.ParseUUID(getFullUUID("0038"))
	attrEndOfWorkoutSummaryDataCharacteristicsUUID, _           = gatt.ParseUUID(getFullUUID("0039"))
	attrAdditionalEndOfWorkoutSummaryDataCharacteristicsUUID, _ = gatt.ParseUUID(getFullUUID("003A"))
	attrHeartRateBeltInfoCharacteristicsUUID, _                 = gatt.ParseUUID(getFullUUID("003B"))
	attrForceCurveDataCharacteristicsUUID, _                    = gatt.ParseUUID(getFullUUID("003D"))
	attrMultiplexedInfoCharacteristicsUUID, _                   = gatt.ParseUUID(getFullUUID("0080"))
)

// sampleRate codes and their notify intervals, per the PM5 BLE spec's
// 0x0034 characteristic (0=1s, 1=500ms default, 2=250ms, 3=100ms).
var sampleIntervals = map[byte]time.Duration{
	0: 1 * time.Second,
	1: 500 * time.Millisecond,
	2: 250 * time.Millisecond,
	3: 100 * time.Millisecond,
}

const defaultSampleRateCode = 1

// notifyLoop repeatedly writes encode(sim.Snapshot()) to n at the current
// sample-rate interval until the client unsubscribes (gatt.Notifier.Done()).
func notifyLoop(n gatt.Notifier, sim *simulator.Simulator, sampleRateCode *uint32, encode func(simulator.LiveState) []byte) {
	for {
		if n.Done() {
			return
		}
		interval, ok := sampleIntervals[byte(atomic.LoadUint32(sampleRateCode))]
		if !ok {
			interval = sampleIntervals[defaultSampleRateCode]
		}
		if _, err := writeNotification(n, encode(sim.Snapshot())); err != nil {
			return
		}
		time.Sleep(interval)
	}
}

// eventNotifyLoop writes encode(sim.Snapshot()) whenever an event fires on
// events (a split boundary or workout-finished signal from the simulator),
// until the client unsubscribes.
func eventNotifyLoop(n gatt.Notifier, sim *simulator.Simulator, events <-chan struct{}, unsubscribe func(), encode func(simulator.LiveState) []byte) {
	defer unsubscribe()
	for {
		if n.Done() {
			return
		}
		select {
		case <-events:
			if _, err := writeNotification(n, encode(sim.Snapshot())); err != nil {
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// NewRowingService advertises rowing service defined by PM5 device, backed by
// sim: every characteristic reflects the same shared, physics-based workout
// simulation rather than independent dummy data.
func NewRowingService(sim *simulator.Simulator) *gatt.Service {
	s := gatt.NewService(attrRowingServiceUUID)

	// Sample rate, shared by every "on-tick" notify loop below.
	var sampleRateCode uint32 = defaultSampleRateCode

	/*
		C2 rowing general status characteristic
	*/
	rowingGenStatusChar := s.AddCharacteristic(attrGeneralStatusCharacteristicsUUID)
	rowingGenStatusChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("General Status Char Notify Request - launching goroutine")
		go notifyLoop(n, sim, &sampleRateCode, rowing.EncodeGeneralStatus)
	})

	/*
		C2 rowing additional status 1 characteristic
	*/
	additionalStatus1Char := s.AddCharacteristic(attrAdditionalStatus1CharacteristicsUUID)
	additionalStatus1Char.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Additional Status 1 Char Notify Request - launching goroutine")
		go notifyLoop(n, sim, &sampleRateCode, rowing.EncodeAdditionalStatus1)
	})

	/*
		C2 rowing additional status 2 characteristic
	*/
	additionalStatus2Char := s.AddCharacteristic(attrAdditionalStatus2CharacteristicsUUID)
	additionalStatus2Char.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Additional Status 2 Char Notify Request - launching goroutine")
		go notifyLoop(n, sim, &sampleRateCode, rowing.EncodeAdditionalStatus2)
	})

	/*
		C2 rowing general status and additional status sample rate characteristic 0x0034
	*/
	sampleRateChar := s.AddCharacteristic(attrSampleRateCharacteristicsUUID)
	sampleRateChar.HandleReadFunc(func(rsp gatt.ResponseWriter, req *gatt.ReadRequest) {
		logrus.Info("Sample Rate Char Read Request")
		rsp.Write([]byte{byte(atomic.LoadUint32(&sampleRateCode))})
	})

	sampleRateChar.HandleWriteFunc(func(req gatt.Request, data []byte) (status byte) {
		logrus.Info("Sample Rate Char Write Request: ", data)
		// Real clients (e.g. ErgData) may pad the write past 1 byte; only
		// the first byte is meaningful. Reject only if it's empty or an
		// unrecognized rate code, matching real PM5 leniency here.
		if len(data) < 1 {
			logrus.Error("Sample Rate Char Write Request: no data")
			return gatt.StatusUnexpectedError
		}
		if _, ok := sampleIntervals[data[0]]; !ok {
			logrus.Errorf("Sample Rate Char Write Request: unknown rate code %v", data[0])
			return gatt.StatusUnexpectedError
		}
		atomic.StoreUint32(&sampleRateCode, uint32(data[0]))
		return gatt.StatusSuccess
	})

	/*
		C2 rowing stroke data characteristic 0x0035
	*/
	strokeDataChar := s.AddCharacteristic(attrStrokeDataCharacteristicsUUID)
	strokeDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Stroke Data Char Notify Request - launching goroutine")
		go notifyLoop(n, sim, &sampleRateCode, rowing.EncodeStrokeData)
	})

	/*
		C2 rowing additional stroke data characteristic 0x0036
	*/
	additionalStrokeDataChar := s.AddCharacteristic(attrAdditionalStrokeDataCharacteristicsUUID)
	additionalStrokeDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Additional Stroke Data Char Notify Request - launching goroutine")
		go notifyLoop(n, sim, &sampleRateCode, rowing.EncodeAdditionalStrokeData)
	})

	/*
		C2 rowing split/interval data characteristic: fires on split
		boundaries rather than a fixed timer.
	*/
	splitIntervalDataChar := s.AddCharacteristic(attrSplitIntervalDataCharacteristicsUUID)
	splitIntervalDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Split/Interval Data Char Notify Request - launching goroutine")
		splitEvents, _, unsubscribe := sim.Subscribe()
		go eventNotifyLoop(n, sim, splitEvents, unsubscribe, rowing.EncodeSplitIntervalData)
	})

	/*
		C2 rowing additional split/interval data characteristic
	*/
	additionalSplitIntervalDataChar := s.AddCharacteristic(attrAdditionalSplitIntervalDataCharacteristicsUUID)
	additionalSplitIntervalDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("Additional Split/Interval Data Char Notify Request - launching goroutine")
		splitEvents, _, unsubscribe := sim.Subscribe()
		go eventNotifyLoop(n, sim, splitEvents, unsubscribe, rowing.EncodeAdditionalSplitIntervalData)
	})

	/*
		C2 rowing end of workout summary data characteristic: fires once
		the simulated workout finishes.
	*/
	endOfWorkoutSummaryDataChar := s.AddCharacteristic(attrEndOfWorkoutSummaryDataCharacteristicsUUID)
	endOfWorkoutSummaryDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("End of workout summary Data Char Notify Request - launching goroutine")
		_, summaryEvents, unsubscribe := sim.Subscribe()
		go eventNotifyLoop(n, sim, summaryEvents, unsubscribe, rowing.EncodeEndOfWorkoutSummary)
	})

	/*
		C2 rowing end of workout additional summary data characteristic
	*/
	additionalEndOfWorkoutSummaryDataChar := s.AddCharacteristic(attrAdditionalEndOfWorkoutSummaryDataCharacteristicsUUID)
	additionalEndOfWorkoutSummaryDataChar.HandleNotifyFunc(func(r gatt.Request, n gatt.Notifier) {
		logrus.Info("End of workout Additional summary Data Char Notify Request - launching goroutine")
		_, summaryEvents, unsubscribe := sim.Subscribe()
		go eventNotifyLoop(n, sim, summaryEvents, unsubscribe, rowing.EncodeAdditionalEndOfWorkoutSummary)
	})

	/*
		C2 rowing heart rate belt information characteristic: no belt is
		paired in this emulator, so there's nothing to notify.
	*/
	heartRateBeltInfoChar := s.AddCharacteristic(attrHeartRateBeltInfoCharacteristicsUUID)
	heartRateBeltInfoChar.HandleReadFunc(func(rsp gatt.ResponseWriter, req *gatt.ReadRequest) {
		rsp.Write(make([]byte, 6))
	})

	/*
		C2 force curve data characteristic: not modeled by this simulator.
	*/
	forceCurveDataChar := s.AddCharacteristic(attrForceCurveDataCharacteristicsUUID)
	forceCurveDataChar.HandleReadFunc(func(rsp gatt.ResponseWriter, req *gatt.ReadRequest) {
		rsp.Write(make([]byte, 2))
	})

	/*
		C2 multiplexed information characteristic

		0x0080 | Up to 20 bytes | READ Permission

		Per spec, an ID is only multiplexed here while the matching
		standalone characteristic notification is NOT enabled; this
		emulator keeps it simple and always serves general status on read.
	*/
	multiplexedInfoChar := s.AddCharacteristic(attrMultiplexedInfoCharacteristicsUUID)
	multiplexedInfoChar.HandleReadFunc(func(rsp gatt.ResponseWriter, req *gatt.ReadRequest) {
		rsp.Write(rowing.EncodeMultiplexed(rowing.IDGeneralStatus, sim.Snapshot()))
	})

	return s
}
