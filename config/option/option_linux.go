package option

import (
	"os"
	"strconv"
	"strings"

	"github.com/bettercap/gatt"
	"github.com/bettercap/gatt/linux/cmd"
)

// detectHCIDeviceID finds the lowest-numbered registered HCI adapter by
// reading /sys/class/bluetooth, rather than relying on gatt.LnxDeviceID's
// own auto-detect (id -1): that mode assumes HCI device indices form a
// contiguous 0-based range and only ever tries index 0, so it fails outright
// once the kernel has assigned a non-zero index to the only adapter present
// -- which happens, for example, after the adapter's raw HCI socket is
// released and bluetoothd re-registers it; Linux doesn't reliably reuse the
// freed index within a boot session. Falls back to -1 (the library's own
// auto-detect) if nothing is found, e.g. on non-Linux or sysfs-less
// environments.
func detectHCIDeviceID() int {
	entries, err := os.ReadDir("/sys/class/bluetooth")
	if err != nil {
		return -1
	}

	best := -1
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "hci") {
			continue
		}
		id, err := strconv.Atoi(e.Name()[len("hci"):])
		if err != nil {
			continue
		}
		if best == -1 || id < best {
			best = id
		}
	}
	return best
}

var DefaultClientOptions = []gatt.Option{
	gatt.LnxMaxConnections(1),
	gatt.LnxDeviceID(detectHCIDeviceID(), true),
}

var DefaultServerOptions = []gatt.Option{
	gatt.LnxMaxConnections(1),
	gatt.LnxDeviceID(detectHCIDeviceID(), true),
	gatt.LnxSetAdvertisingParameters(&cmd.LESetAdvertisingParameters{
		AdvertisingIntervalMin: 0x00f4,
		AdvertisingIntervalMax: 0x00f4,
		AdvertisingChannelMap:  0x7,
	}),
}
