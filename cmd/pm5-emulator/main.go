package main

import (
	"flag"

	"pm5-emulator/emulator"
	_ "pm5-emulator/log"
)

func main() {
	csvPath := flag.String("csv", "", "path to a recorded PM5 session CSV to replay instead of the built-in physics simulator")
	flag.Parse()

	em := emulator.NewEmulator() //factory method
	em.RunEmulator(*csvPath)
	select {}
}
