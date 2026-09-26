package emulator

import (
	"fmt"
	"time"

	"github.com/bettercap/gatt"
	"github.com/sirupsen/logrus"
	"pm5-emulator/config"
	"pm5-emulator/service"
	"pm5-emulator/simulator"
	"pm5-emulator/sm"
)

// autoStartDelay is how long after a central connects before the emulator
// drives itself into an active, simulated row -- long enough for the client
// to finish initial service discovery/reads first.
const autoStartDelay = 1 * time.Second

// Emulator emulates PM5 indoor rower machine
type Emulator struct {
	device       gatt.Device
	stateMachine *sm.StateMachine
}

// RunEmulator registers handlers and starts advertising services. When
// csvPath is non-empty, a recorded PM5 session is replayed from that file
// instead of the built-in physics simulator driving the workout data.
func (em *Emulator) RunEmulator(csvPath string) {

	// Single, device-wide state machine and workout simulator shared by every
	// service/characteristic, instead of each characteristic tracking its own.
	em.stateMachine.Reset()
	sim := simulator.NewSimulator(em.stateMachine)
	if csvPath != "" {
		timeline, err := simulator.LoadCSVTimeline(csvPath)
		if err != nil {
			logrus.Fatalf("failed to load CSV session %q: %v", csvPath, err)
		}
		logrus.Infof("replaying recorded session from %q", csvPath)
		go timeline.Play(sim)
	} else {
		go sim.Run()
	}

	//register optional handlers
	em.registerHandlers(sim)

	// handler for monitoring config state.
	onStateChanged := func(d gatt.Device, s gatt.State) {
		fmt.Printf("State: %s\n", s)
		switch s {
		case gatt.StatePoweredOn:
			// Setup GAP and GATT services for PM5
			_ = d.AddService(service.NewGapService(config.NAME))
			_ = d.AddService(service.NewGattService())

			// Setup Device info service for PM5
			s1 := service.NewDevInfoService()
			d.AddService(s1)

			s2 := service.NewControlService(em.stateMachine, sim)
			d.AddService(s2)

			s3 := service.NewRowingService(sim)
			d.AddService(s3)

			// Advertise config name and service's UUIDs.
			d.AdvertiseNameAndServices(config.NAME, []gatt.UUID{gatt.MustParseUUID("CE060000-43E5-11E4-916C-0800200C9A66")})

		default:
		}
	}

	em.device.Init(onStateChanged)
}

// registerHandlers registers optional handlers for handling device connection and disconnection
func (em *Emulator) registerHandlers(sim *simulator.Simulator) {
	// Register optional handlers.
	em.device.Handle(
		gatt.PeripheralConnected(func(p gatt.Peripheral, err error) {
			logrus.Info("|Peripheral Connected|: ")
			logrus.Info("ID: ", p.ID())
			logrus.Info("Device: ", p.Device())
			logrus.Info("Name: ", p.Name())
		}),
		gatt.CentralConnected(func(c gatt.Central) {
			logrus.Info("|Device Connected| ID=> ", c.ID())
			logrus.Info("MTU: ", c.MTU())
			go em.autoStartWorkout(sim)
		}),
		gatt.CentralDisconnected(func(c gatt.Central) {
			logrus.Info("|Device Disconnected| ID=> ", c.ID())
			logrus.Info("MTU: ", c.MTU())
		}),
	)
}

// autoStartWorkout drives the emulator into an active, simulated row on its
// own shortly after a client connects. A real PM5 becomes "in use" based on
// the flywheel actually spinning, independent of anything the BLE client
// sends; a passive/read-only client connecting to this emulator would
// otherwise see nothing but zeroed-out data forever, since nothing else
// would ever issue the CSAFE GOIDLE/GOHAVEID/GOINUSE sequence.
func (em *Emulator) autoStartWorkout(sim *simulator.Simulator) {
	time.Sleep(autoStartDelay)

	em.stateMachine.Reset()
	sim.Reset()
	for _, cmd := range []byte{config.CSAFE_GOIDLE_CMD, config.CSAFE_GOHAVEID_CMD, config.CSAFE_GOINUSE_CMD} {
		if err := em.stateMachine.Update(cmd); err != nil {
			logrus.Warnf("auto-start: cmd 0x%x rejected: %v", cmd, err)
			return
		}
	}
	logrus.Info("auto-start: simulated row started")
}
