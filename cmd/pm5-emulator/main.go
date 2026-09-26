package main

import (
	"flag"
	"log"

	"pm5-emulator/emulator"
	_ "pm5-emulator/log"
)

func main() {
	csvPath := flag.String("csv", "", "path to a recorded PM5 session CSV to replay instead of the built-in physics simulator")
	startMode := flag.String("start", "auto", `how a workout begins: "auto" (default, starts by itself ~1s after a client connects) or "manual" (wait for the client to send CSAFE GOIDLE/GOHAVEID/GOINUSE itself)`)
	flag.Parse()

	switch *startMode {
	case "auto", "manual":
	default:
		log.Fatalf(`invalid -start value %q: must be "auto" or "manual"`, *startMode)
	}

	em := emulator.NewEmulator() //factory method
	em.RunEmulator(*csvPath, *startMode == "auto")
	select {}
}
