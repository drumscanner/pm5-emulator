package emulator

import (
	"fmt"
	"github.com/bettercap/gatt"
	"github.com/sirupsen/logrus"
	"pm5-emulator/config"
	"pm5-emulator/service"
	"pm5-emulator/simulator"
	"pm5-emulator/sm"
)

// Emulator emulates PM5 indoor rower machine
type Emulator struct {
	device       gatt.Device
	stateMachine *sm.StateMachine
}

// RunEmulator registers handlers and starts advertising services
func (em *Emulator) RunEmulator() {

	//register optional handlers
	em.registerHandlers()

	// Single, device-wide state machine and workout simulator shared by every
	// service/characteristic, instead of each characteristic tracking its own.
	em.stateMachine.Reset()
	sim := simulator.NewSimulator(em.stateMachine)
	go sim.Run()

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
func (em *Emulator) registerHandlers() {
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
		}),
		gatt.CentralDisconnected(func(c gatt.Central) {
			logrus.Info("|Device Disconnected| ID=> ", c.ID())
			logrus.Info("MTU: ", c.MTU())
		}),
	)
}
