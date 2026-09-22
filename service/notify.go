package service

import (
	"sync"

	"github.com/bettercap/gatt"
)

// notifyMu serializes every outgoing BLE notification across all
// characteristics/services. bettercap/gatt's underlying L2CAP write
// (linux/l2cap.go conn.write) fragments a notification into raw HCI ACL
// packets with no locking of its own; if two goroutines call Notifier.Write
// concurrently on the same connection, their fragments can interleave on
// the wire and corrupt the L2CAP stream. This emulator only ever serves one
// central at a time (gatt.LnxMaxConnections(1) in config/option), so a
// single global mutex is enough to prevent that -- strict clients (e.g.
// iOS's CoreBluetooth stack, which ErgData is built on) react to a
// corrupted stream by tearing the connection down immediately.
var notifyMu sync.Mutex

func writeNotification(n gatt.Notifier, data []byte) (int, error) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	return n.Write(data)
}
